package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	genericapiserver "k8s.io/apiserver/pkg/server"
)

func TestReloadsServingCertificate(t *testing.T) {
	dir := t.TempDir()
	first := writeServingCert(t, dir)
	kubeconfig := fakeKubeAPIServer(t)
	o := newOptions()
	err := newCommand(o).ParseFlags([]string{
		"--tls-cert-file=" + filepath.Join(dir, "tls.crt"),
		"--tls-private-key-file=" + filepath.Join(dir, "tls.key"),
		"--authentication-kubeconfig=" + kubeconfig,
		"--authentication-skip-lookup",
		"--authorization-kubeconfig=" + kubeconfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	o.SecureServing.Listener = listener
	c, err := o.config()
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Complete(nil).New("test", genericapiserver.NewEmptyDelegate())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	var runErr error
	go func() {
		defer close(stopped)
		runErr = s.PrepareRun().RunWithContext(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
		if runErr != nil {
			t.Error(runErr)
		}
	})

	waitForServedCert(t, listener.Addr().String(), stopped, first)
	second := writeServingCert(t, dir)
	waitForServedCert(t, listener.Addr().String(), stopped, second)
}

func waitForServedCert(t *testing.T, addr string, stopped <-chan struct{}, want *x509.Certificate) {
	t.Helper()
	dialer := &net.Dialer{Timeout: time.Second}
	var served *big.Int
	var dialErr error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		select {
		case <-stopped:
			t.Fatal("server stopped")
		default:
		}
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			dialErr = err
			continue
		}
		got := conn.ConnectionState().PeerCertificates[0]
		_ = conn.Close()
		if got.Equal(want) {
			return
		}
		served = got.SerialNumber
	}
	t.Fatalf("served serial %v (last dial error: %v), want %v", served, dialErr, want.SerialNumber)
}

// writeServingCert updates dir/tls.crt and dir/tls.key like kubelet updates a Secret volume:
// it writes a new directory, repoints the ..data symlink to it, and removes the old one.
func writeServingCert(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: []string{"localhost"}, NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	version, err := os.MkdirTemp(dir, "..version")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, version, "tls.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, version, "tls.key", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	data := filepath.Join(dir, "..data")
	old, _ := os.Readlink(data)
	if err := os.Symlink(filepath.Base(version), data+"_tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(data+"_tmp", data); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrExist) {
			t.Fatal(err)
		}
	}
	if old != "" {
		if err := os.RemoveAll(filepath.Join(dir, old)); err != nil {
			t.Fatal(err)
		}
	}
	return cert
}
