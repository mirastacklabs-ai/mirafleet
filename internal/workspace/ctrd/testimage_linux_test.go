//go:build linux && integration

package ctrd

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
)

// Hermetic test image: an OCI image layout tarball built at test time from this test binary
// (installed as /bin/init) plus a minimal /etc/passwd and /etc/group (uid/gid 1000). Nothing is
// pulled; the tarball is fed to containerd's client.Import.

const (
	testImageRef = "mirafleet-test/init:1"
	// testInitEnv makes the test binary act as the container's init instead of running tests.
	testInitEnv = "MIRAFLEET_CTRD_TESTINIT"
)

// buildTestImage returns the OCI layout tarball and the image reference it carries.
func buildTestImage() (data []byte, ref string, err error) {
	self, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		return nil, "", fmt.Errorf("read test binary: %w", err)
	}

	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	add := func(h *tar.Header, body []byte) error {
		h.Format = tar.FormatPAX
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	passwd := []byte("root:x:0:0:root:/root:/bin/init\nworker:x:1000:1000:worker:/tmp:/bin/init\n")
	group := []byte("root:x:0:\nworker:x:1000:\n")
	for _, d := range []string{"bin/", "etc/", "tmp/"} {
		mode := int64(0o755)
		if d == "tmp/" {
			mode = 0o1777
		}
		if err := add(&tar.Header{Typeflag: tar.TypeDir, Name: d, Mode: mode}, nil); err != nil {
			return nil, "", err
		}
	}
	if err := add(&tar.Header{Typeflag: tar.TypeReg, Name: "bin/init", Mode: 0o555}, self); err != nil {
		return nil, "", err
	}
	if err := add(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/passwd", Mode: 0o644}, passwd); err != nil {
		return nil, "", err
	}
	if err := add(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/group", Mode: 0o644}, group); err != nil {
		return nil, "", err
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	layerBytes := layer.Bytes()
	layerDigest := sha256Hex(layerBytes) // uncompressed layer: digest == diff_id

	cfg, _ := json.Marshal(map[string]any{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"config": map[string]any{
			"User":       "1000:1000",
			"Entrypoint": []string{"/bin/init"},
			"WorkingDir": "/",
			"Env":        []string{"PATH=/bin"},
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + layerDigest}},
	})
	cfgDigest := sha256Hex(cfg)

	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    "sha256:" + cfgDigest,
			"size":      len(cfg),
		},
		"layers": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.layer.v1.tar",
			"digest":    "sha256:" + layerDigest,
			"size":      len(layerBytes),
		}},
	})
	manifestDigest := sha256Hex(manifest)

	index, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"manifests": []any{map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    "sha256:" + manifestDigest,
			"size":      len(manifest),
			"platform":  map[string]any{"architecture": runtime.GOARCH, "os": "linux"},
			"annotations": map[string]any{
				"io.containerd.image.name":          ref0(),
				"org.opencontainers.image.ref.name": "1",
			},
		}},
	})

	var out bytes.Buffer
	ow := tar.NewWriter(&out)
	put := func(name string, body []byte) error {
		if err := ow.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return err
		}
		_, err := io.Copy(ow, bytes.NewReader(body))
		return err
	}
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{"index.json", index},
		{"blobs/sha256/" + layerDigest, layerBytes},
		{"blobs/sha256/" + cfgDigest, cfg},
		{"blobs/sha256/" + manifestDigest, manifest},
	} {
		if err := put(f.name, f.body); err != nil {
			return nil, "", err
		}
	}
	if err := ow.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), ref0(), nil
}

func ref0() string { return testImageRef }

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
