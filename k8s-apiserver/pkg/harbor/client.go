// Package harbor reads repositories and artifacts from the Harbor v2.0 REST API.
package harbor

import (
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
	ErrNotFound            = errors.New("not found in harbor")
	ErrCredentialsRejected = errors.New("harbor rejected the robot account credentials")
	ErrUnavailable         = errors.New("harbor is unavailable")
)

type Repository struct {
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	ArtifactCount int64     `json:"artifact_count"`
	PullCount     int64     `json:"pull_count"`
	CreationTime  time.Time `json:"creation_time"`
	UpdateTime    time.Time `json:"update_time"`
}

type Artifact struct {
	Digest            string            `json:"digest"`
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
	Name      string    `json:"name"`
	PushTime  time.Time `json:"push_time"`
	PullTime  time.Time `json:"pull_time"`
	Immutable bool      `json:"immutable"`
}

type Client struct {
	baseURL            string
	username, password string
	http               *http.Client
	pageSize           int
}

type Option func(*Client)

// WithPageSize sets how many items each list request asks for. Harbor allows at most 100.
func WithPageSize(n int) Option {
	return func(c *Client) { c.pageSize = n }
}

func NewClient(baseURL, username, password string, httpClient *http.Client, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("harbor URL %q must use http or https", baseURL)
	}
	c := &Client{
		baseURL:  strings.TrimSuffix(baseURL, "/") + "/api/v2.0",
		username: username,
		password: password,
		http:     httpClient,
		pageSize: 100,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// NewHTTPClient returns a client that trusts caBundle (PEM) in addition to the system roots.
func NewHTTPClient(caBundle []byte, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
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
	return list[Repository](ctx, c, projectPath(project)+"/repositories", nil)
}

// GetRepository gets a repository by its name within the project, such as "team/app".
func (c *Client) GetRepository(ctx context.Context, project, repository string) (*Repository, error) {
	var r Repository
	return &r, c.get(ctx, repositoryPath(project, repository), nil, &r)
}

func (c *Client) ListArtifacts(ctx context.Context, project, repository string) ([]Artifact, error) {
	return list[Artifact](ctx, c, repositoryPath(project, repository)+"/artifacts", withTags)
}

func (c *Client) GetArtifact(ctx context.Context, project, repository, digest string) (*Artifact, error) {
	var a Artifact
	return &a, c.get(ctx, repositoryPath(project, repository)+"/artifacts/"+url.PathEscape(digest), withTags, &a)
}

var withTags = url.Values{"with_tag": {"true"}}

func projectPath(project string) string {
	return "/projects/" + url.PathEscape(project)
}

// repositoryPath encodes the repository name twice, as Harbor requires for names containing "/".
func repositoryPath(project, repository string) string {
	return projectPath(project) + "/repositories/" + url.PathEscape(url.PathEscape(repository))
}

func list[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(c.pageSize)}}
		for k, v := range query {
			q[k] = v
		}
		var items []T
		resp, err := c.do(ctx, path, q, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		total, err := strconv.Atoi(resp.Header.Get("X-Total-Count"))
		if len(items) < c.pageSize || (err == nil && len(all) >= total) {
			return all, nil
		}
	}
}

func (c *Client) get(ctx context.Context, path string, query url.Values, into any) error {
	_, err := c.do(ctx, path, query, into)
	return err
}

func (c *Client) do(ctx context.Context, path string, query url.Values, into any) (*http.Response, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, responseError(req, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return nil, fmt.Errorf("decoding GET %s: %w", req.URL.Path, err)
	}
	return resp, nil
}

func responseError(req *http.Request, resp *http.Response) error {
	var kind error
	switch {
	case resp.StatusCode == http.StatusNotFound:
		kind = ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		kind = ErrCredentialsRejected
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		kind = ErrUnavailable
	default:
		kind = errors.New("unexpected response from harbor")
	}

	var body struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &body) == nil && len(body.Errors) > 0 {
		msg = body.Errors[0].Message
	}
	return fmt.Errorf("%w: GET %s: %s: %s", kind, req.URL.Path, resp.Status, msg)
}
