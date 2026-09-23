// Package harbor is a client for the Harbor v2.0 REST API.
package harbor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrNotFound     = errors.New("not found in harbor")
	ErrUnauthorized = errors.New("harbor rejected the robot account credentials")
	ErrForbidden    = errors.New("harbor denied the robot account access")
	ErrUnavailable  = errors.New("harbor is unavailable")
	ErrBadRequest   = errors.New("harbor rejected the request as invalid")
	ErrConflict     = errors.New("harbor reported a conflict")
	ErrPrecondition = errors.New("harbor reported a failed precondition")
)

const pageSize = 100

// Limits that stop a misbehaving server from exhausting memory.
var (
	maxPages         = 1000
	maxResponseBytes = int64(16 << 20)
)

type Repository struct {
	ID int64 `json:"id"`
	// Name is the full name, starting with the project.
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	ArtifactCount int64     `json:"artifact_count"`
	PullCount     int64     `json:"pull_count"`
	CreationTime  time.Time `json:"creation_time"`
	UpdateTime    time.Time `json:"update_time"`
}

type Artifact struct {
	ID     int64  `json:"id"`
	Digest string `json:"digest"`
	// RepositoryName is the full name, starting with the project.
	RepositoryName    string            `json:"repository_name"`
	Type              string            `json:"type"`
	MediaType         string            `json:"media_type"`
	ManifestMediaType string            `json:"manifest_media_type"`
	ArtifactType      string            `json:"artifact_type"`
	Size              int64             `json:"size"`
	PushTime          time.Time         `json:"push_time"`
	PullTime          time.Time         `json:"pull_time"`
	Annotations       map[string]string `json:"annotations"`
	References        []Reference       `json:"references"`
	Tags              []Tag             `json:"tags"`
}

type Reference struct {
	ChildDigest string    `json:"child_digest"`
	Platform    *Platform `json:"platform"`
}

type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant"`
}

type Tag struct {
	Name     string    `json:"name"`
	PushTime time.Time `json:"push_time"`
	PullTime time.Time `json:"pull_time"`
}

// Credentials returns a robot account's name and secret.
type Credentials func() (username, password string, err error)

type Client struct {
	baseURL     string
	credentials Credentials
	http        *http.Client
}

// NewClient returns a client that authenticates as a robot account, getting its credentials for each request.
// It refuses redirects, so the credentials go only to baseURL.
func NewClient(baseURL string, credentials Credentials, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("harbor URL %q must be http or https with a host and no query", baseURL)
	}
	if credentials == nil {
		return nil, errors.New("harbor client needs credentials")
	}
	if httpClient == nil {
		return nil, errors.New("harbor client needs an HTTP client")
	}
	noRedirects := *httpClient
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		baseURL:     strings.TrimSuffix(baseURL, "/") + "/api/v2.0",
		credentials: credentials,
		http:        &noRedirects,
	}, nil
}

// NewHTTPClient returns a client that trusts caBundle (PEM) in addition to the system roots.
func NewHTTPClient(caBundle []byte, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 16
	if len(caBundle) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(caBundle) {
			return nil, errors.New("CA bundle contains no PEM certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func (c *Client) ListRepositories(ctx context.Context, project string) ([]Repository, error) {
	return list(ctx, c, projectPath(project)+"/repositories", url.Values{"sort": {"repository_id"}},
		func(r Repository) int64 { return r.ID })
}

// ListArtifacts lists a repository's artifacts, leaving out accessories and the children of an index.
func (c *Client) ListArtifacts(ctx context.Context, project, repository string) ([]Artifact, error) {
	return list(ctx, c, repositoryPath(project, repository)+"/artifacts", url.Values{"sort": {"id"}, "with_tag": {"true"}},
		func(a Artifact) int64 { return a.ID })
}

func projectPath(project string) string {
	return "/projects/" + url.PathEscape(project)
}

// repositoryPath encodes the repository name twice, as Harbor requires for names containing "/".
func repositoryPath(project, repository string) string {
	return projectPath(project) + "/repositories/" + url.PathEscape(url.PathEscape(repository))
}

// listAttempts is how many times list tries to get pages that did not shift.
const listAttempts = 3

// listRetryWait is how long list waits before it starts over.
var listRetryWait = time.Second

var errShifted = errors.New("pages shifted during the list")

// list gets every page, sorted by ID so that items created during the list land on its last page.
// A deletion shifts later pages, which skips an item, so list starts over when the total count drops.
func list[T any](ctx context.Context, c *Client, path string, query url.Values, id func(T) int64) ([]T, error) {
	for attempt := range listAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%w: %w", ErrUnavailable, ctx.Err())
			case <-time.After(listRetryWait):
			}
		}
		all, err := listOnce(ctx, c, path, query, id)
		if !errors.Is(err, errShifted) {
			return all, err
		}
	}
	return nil, fmt.Errorf("GET %s: %w %d times", path, errShifted, listAttempts)
}

// listOnce gets every page, dropping items that a shift repeated.
func listOnce[T any](ctx context.Context, c *Client, path string, query url.Values, id func(T) int64) ([]T, error) {
	var all []T
	seen := map[int64]bool{}
	lastTotal := 0
	for page := 1; page <= maxPages; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}}
		for k, v := range query {
			q[k] = v
		}
		var items []T
		header, err := c.do(ctx, http.MethodGet, path, q, nil, http.StatusOK, &items)
		if err != nil {
			return nil, err
		}
		total := totalCount(header)
		if page > 1 && total < lastTotal {
			return nil, errShifted
		}
		lastTotal = total
		for _, item := range items {
			if !seen[id(item)] {
				seen[id(item)] = true
				all = append(all, item)
			}
		}
		if len(items) < pageSize || (total >= 0 && page*pageSize >= total) {
			if total >= 0 && len(all) != total {
				return nil, errShifted
			}
			return all, nil
		}
	}
	return nil, fmt.Errorf("GET %s: more than %d pages", path, maxPages)
}

// totalCount returns the X-Total-Count header, or -1 if it has none.
func totalCount(header http.Header) int {
	total, err := strconv.Atoi(header.Get("X-Total-Count"))
	if err != nil {
		return -1
	}
	return total
}

// do sends body as JSON if it is not nil, and decodes the response into `into` if it is not nil.
// A status other than want is an error.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, want int, into any) (http.Header, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding %s %s: %w", method, path, err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return nil, err
	}
	username, password, err := c.credentials()
	if err != nil {
		return nil, fmt.Errorf("getting harbor credentials: %w", err)
	}
	req.SetBasicAuth(username, password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != want {
		return nil, responseError(req, resp)
	}
	if into != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(into); err != nil {
			return nil, fmt.Errorf("decoding %s %s: %w", method, req.URL.Path, err)
		}
	}
	return resp.Header, nil
}

func responseError(req *http.Request, resp *http.Response) error {
	var kind error
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		kind = ErrBadRequest
	case resp.StatusCode == http.StatusNotFound:
		kind = ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		kind = ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		kind = ErrForbidden
	case resp.StatusCode == http.StatusConflict:
		kind = ErrConflict
	case resp.StatusCode == http.StatusPreconditionFailed:
		kind = ErrPrecondition
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		kind = ErrUnavailable
	default:
		kind = errors.New("unexpected response from harbor")
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	msg := strings.ToValidUTF8(string(raw[:min(len(raw), 256)]), "")
	if json.Unmarshal(raw, &body) == nil && len(body.Errors) > 0 {
		msg = body.Errors[0].Message
	}
	err := fmt.Errorf("%w: %s %s: %s", kind, req.Method, req.URL.Path, resp.Status)
	if msg = strings.TrimSpace(msg); msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	return err
}
