//go:build linux && integration

package gitlab

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type image struct {
	manifest []byte
	digest   string
}

func sum(b []byte) string { h := sha256.Sum256(b); return fmt.Sprintf("sha256:%x", h) }

// AddImage publishes a tiny valid single-layer image as name:tag and returns the manifest digest.
func (s *Server) AddImage(t testing.TB, name, tag string) string {
	t.Helper()
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	body := []byte("hello from the fake registry\n")
	if err := tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(tb.Bytes())
	zw.Close()
	cfg, _ := json.Marshal(map[string]any{
		"architecture": "amd64", "os": "linux",
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{sum(tb.Bytes())}},
	})
	desc := func(mt string, b []byte) map[string]any {
		return map[string]any{"mediaType": mt, "digest": sum(b), "size": len(b)}
	}
	mf, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": desc("application/vnd.oci.image.config.v1+json", cfg),
		"layers": []any{desc("application/vnd.oci.image.layer.v1.tar+gzip", gz.Bytes())},
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[sum(cfg)], s.blobs[sum(gz.Bytes())] = cfg, gz.Bytes()
	img := image{manifest: mf, digest: sum(mf)}
	s.mfs[name+":"+tag], s.mfs[name+":"+img.digest] = img, img
	return img.digest
}

// tokenEndpoint is the token service named in the 401 challenge: basic auth with the registry
// credentials in, an opaque bearer token scoped to the requested repository out.
func (s *Server) tokenEndpoint(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	good := ok && eq(u, s.RegistryUser) && eq(p, s.RegistryPassword)
	if ok {
		s.record(Event{Transport: "registry-basic", Identity: u, Credential: p, OK: good})
	}
	if !good {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry token service"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rb := make([]byte, 16)
	if _, err := rand.Read(rb); err != nil {
		http.Error(w, "entropy", http.StatusInternalServerError)
		return
	}
	tok := fmt.Sprintf("jwt.%x", rb)
	s.mu.Lock()
	s.tokens[tok] = r.URL.Query().Get("scope")
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"token": tok, "access_token": tok, "expires_in": 300})
}

// registry serves /v2/: a 401 Bearer challenge until a token from tokenEndpoint is presented,
// then manifests and blobs of images added with AddImage.
func (s *Server) registry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	name, kind, ref := splitV2(r.URL.Path)
	scope := ""
	if name != "" {
		scope = "repository:" + name + ":pull"
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	granted, known := s.tokens[tok]
	s.mu.Unlock()
	if tok == r.Header.Get("Authorization") || !known || (scope != "" && granted != scope) {
		if tok != r.Header.Get("Authorization") {
			s.record(Event{Transport: "registry-bearer", Credential: tok, OK: false})
		}
		ch := fmt.Sprintf(`Bearer realm="https://%s/jwt/auth",service="fake-registry"`, r.Host)
		if scope != "" {
			ch += fmt.Sprintf(`,scope="%s"`, scope)
		}
		w.Header().Set("WWW-Authenticate", ch)
		http.Error(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`, http.StatusUnauthorized)
		return
	}
	s.record(Event{Transport: "registry-bearer", Credential: tok, OK: true})
	switch kind {
	case "":
		w.Write([]byte("{}"))
	case "manifests":
		s.mu.Lock()
		img, ok := s.mfs[name+":"+ref]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", img.digest)
		w.Write(img.manifest)
	case "blobs":
		s.mu.Lock()
		b, ok := s.blobs[ref]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", ref)
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// splitV2 splits /v2/<name>/{manifests|blobs}/<ref>; name may contain slashes.
func splitV2(p string) (name, kind, ref string) {
	p = strings.TrimPrefix(p, "/v2/")
	for _, k := range []string{"manifests", "blobs"} {
		if i := strings.Index(p, "/"+k+"/"); i > 0 {
			return p[:i], k, p[i+len(k)+2:]
		}
	}
	return "", "", ""
}
