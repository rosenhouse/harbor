//go:build e2e

package e2e

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const apiServiceName = "v1alpha1.harbor.goharbor.io"

func secretData(t *testing.T, namespace, secret, key string) []byte {
	t.Helper()
	b64 := mustKubectl(t, "-n", namespace, "get", "secret", secret, "-o", "jsonpath={.data."+strings.ReplaceAll(key, ".", `\.`)+"}")
	v, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

type apiService struct {
	Spec struct {
		CABundle              []byte `json:"caBundle"`
		InsecureSkipTLSVerify bool   `json:"insecureSkipTLSVerify"`
	} `json:"spec"`
	Status struct {
		Conditions []metav1.Condition `json:"conditions"`
	} `json:"status"`
}

func getAPIService(t *testing.T) apiService {
	t.Helper()
	var s apiService
	if err := json.Unmarshal([]byte(mustKubectl(t, "get", "apiservice", apiServiceName, "-o", "json")), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func checkVerifiesServingCertificate(t *testing.T, s apiService) {
	t.Helper()
	if s.Spec.InsecureSkipTLSVerify {
		t.Error("insecureSkipTLSVerify is true")
	}
	if want := secretData(t, "harbor-apiserver", "harbor-apiserver-tls", "ca.crt"); len(want) == 0 || !bytes.Equal(s.Spec.CABundle, want) {
		t.Errorf("caBundle %q, want ca.crt from Secret harbor-apiserver-tls %q", s.Spec.CABundle, want)
	}
}

func TestAPIServiceVerifiesServingCertificate(t *testing.T) {
	s := getAPIService(t)
	checkVerifiesServingCertificate(t, s)
	if !meta.IsStatusConditionTrue(s.Status.Conditions, "Available") {
		t.Errorf("conditions %+v, want Available", s.Status.Conditions)
	}
}

// genServingCert runs hack/gen-serving-cert.sh and returns its stdout.
func genServingCert(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("../../hack/gen-serving-cert.sh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gen-serving-cert.sh %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

func TestGenServingCertTurnsOnVerification(t *testing.T) {
	mustKubectl(t, "patch", "apiservice", apiServiceName, "--type=merge", "-p", `{"spec":{"caBundle":null,"insecureSkipTLSVerify":true}}`)
	if out := genServingCert(t); out != "" {
		t.Errorf("printed %q, want nothing", out)
	}
	checkVerifiesServingCertificate(t, getAPIService(t))
}

func TestGenServingCertRenewsOnlyTheServingCertificate(t *testing.T) {
	ns := newNamespace(t)
	missingAPIService := "v1." + ns + ".example.com"
	caBundle := genServingCert(t, ns, missingAPIService)
	caPEM, err := base64.StdEncoding.DecodeString(strings.TrimSpace(caBundle))
	if err != nil {
		t.Fatalf("printed %q: %v", caBundle, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatalf("printed %q, want a base64 PEM certificate", caBundle)
	}
	ca, err := tls.X509KeyPair(secretData(t, ns, "harbor-apiserver-ca", "tls.crt"), secretData(t, ns, "harbor-apiserver-ca", "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	checkServingCert := func(state string) []byte {
		t.Helper()
		if got := secretData(t, ns, "harbor-apiserver-tls", "ca.crt"); !bytes.Equal(got, caPEM) {
			t.Errorf("%s: ca.crt %q, want %q", state, got, caPEM)
		}
		crt := secretData(t, ns, "harbor-apiserver-tls", "tls.crt")
		block, _ := pem.Decode(crt)
		if block == nil {
			t.Fatalf("%s: tls.crt %q", state, crt)
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		host := "harbor-apiserver." + ns + ".svc"
		for _, name := range []string{host, host + ".cluster.local"} {
			if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, CurrentTime: time.Now().Add(31 * 24 * time.Hour)}); err != nil {
				t.Errorf("%s: %s in 31 days: %v", state, name, err)
			}
		}
		return crt
	}

	valid := checkServingCert("new")
	if got := genServingCert(t, ns, missingAPIService); got != caBundle {
		t.Errorf("rerun printed %q, want %q", got, caBundle)
	}
	if !bytes.Equal(checkServingCert("rerun"), valid) {
		t.Error("rerun replaced a valid serving certificate")
	}

	for _, tc := range []struct {
		state   string
		replace func()
	}{
		{"deleted", func() { mustKubectl(t, "-n", ns, "delete", "secret", "harbor-apiserver-tls") }},
		{"expiring", func() { putServingCert(t, ns, &ca, 24*time.Hour) }},
		{"another issuer", func() { putServingCert(t, ns, nil, 365*24*time.Hour) }},
	} {
		tc.replace()
		if got := genServingCert(t, ns, missingAPIService); got != caBundle {
			t.Errorf("%s: printed %q, want %q", tc.state, got, caBundle)
		}
		checkServingCert(tc.state)
	}
}

// putServingCert replaces Secret harbor-apiserver-tls with a certificate signed by issuer, or self-signed if issuer is nil.
func putServingCert(t *testing.T, ns string, issuer *tls.Certificate, validFor time.Duration) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		DNSNames:     []string{"harbor-apiserver." + ns + ".svc"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validFor),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	parent, signer := template, crypto.Signer(key)
	if issuer != nil {
		parent, signer = issuer.Leaf, issuer.PrivateKey.(crypto.Signer)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	crtFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(crtFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	mustKubectl(t, "-n", ns, "delete", "secret", "harbor-apiserver-tls")
	mustKubectl(t, "-n", ns, "create", "secret", "tls", "harbor-apiserver-tls", "--cert="+crtFile, "--key="+keyFile)
}

// manifest holds the fields that connect the objects of deploy/cert-manager.
type manifest struct {
	Kind     string
	Metadata struct {
		Name, Namespace string
		Annotations     map[string]string
	}
	Spec struct {
		IsCA                  bool
		SecretName            string
		DNSNames              []string
		IssuerRef             struct{ Name string }
		SelfSigned            *struct{}
		CA                    *struct{ SecretName string }
		InsecureSkipTLSVerify bool
		Service               struct{ Name, Namespace string }
		Template              struct {
			Spec struct {
				Volumes []struct{ Secret struct{ SecretName string } }
			}
		}
	}
}

func TestCertManagerOverlayInjectsTheServingCA(t *testing.T) {
	objects := map[string]manifest{}
	d := yaml.NewYAMLOrJSONDecoder(strings.NewReader(mustKubectl(t, "kustomize", "../../deploy/cert-manager")), 4096)
	for {
		var m manifest
		if err := d.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		objects[m.Kind+"/"+m.Metadata.Namespace+"/"+m.Metadata.Name] = m
	}
	s := objects["APIService//"+apiServiceName]
	ns := s.Spec.Service.Namespace
	host := s.Spec.Service.Name + "." + ns + ".svc"
	if s.Spec.InsecureSkipTLSVerify {
		t.Error("APIService insecureSkipTLSVerify is true")
	}

	serving := objects["Certificate/"+s.Metadata.Annotations["cert-manager.io/inject-ca-from"]]
	if serving.Metadata.Namespace != ns || !slices.Equal(serving.Spec.DNSNames, []string{host, host + ".cluster.local"}) {
		t.Errorf("APIService injects the CA of Certificate %s/%s with DNS names %v, want one in %s for %s", serving.Metadata.Namespace, serving.Metadata.Name, serving.Spec.DNSNames, ns, host)
	}
	var volumes []string
	for _, v := range objects["Deployment/"+ns+"/"+s.Spec.Service.Name].Spec.Template.Spec.Volumes {
		volumes = append(volumes, v.Secret.SecretName)
	}
	if !slices.Contains(volumes, serving.Spec.SecretName) {
		t.Errorf("Deployment mounts Secrets %v, want %q", volumes, serving.Spec.SecretName)
	}

	caIssuer := objects["Issuer/"+ns+"/"+serving.Spec.IssuerRef.Name]
	if caIssuer.Spec.CA == nil {
		t.Fatalf("serving Certificate issuer %q is not a CA Issuer", serving.Spec.IssuerRef.Name)
	}
	for _, ca := range objects {
		if ca.Kind == "Certificate" && ca.Metadata.Namespace == ns && ca.Spec.SecretName == caIssuer.Spec.CA.SecretName {
			if !ca.Spec.IsCA || objects["Issuer/"+ns+"/"+ca.Spec.IssuerRef.Name].Spec.SelfSigned == nil {
				t.Errorf("Certificate %s is not a self-signed CA", ca.Metadata.Name)
			}
			return
		}
	}
	t.Errorf("no Certificate for the CA Issuer's Secret %q", caIssuer.Spec.CA.SecretName)
}
