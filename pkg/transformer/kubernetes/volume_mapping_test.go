/*
Copyright 2026 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubernetes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubernetes/kompose/pkg/kobject"
	"github.com/kubernetes/kompose/pkg/loader/compose"
	appsv1 "k8s.io/api/apps/v1"
	api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// convertCompose loads a minimal compose file from a temp dir and transforms
// it with the given in-memory options. No cluster or real storage is touched.
func convertCompose(t *testing.T, dir string, composeFile string, opt kobject.ConvertOptions) ([]runtime.Object, kobject.KomposeObject) {
	t.Helper()
	path := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(path, []byte(composeFile), 0644); err != nil {
		t.Fatalf("unable to write compose file: %v", err)
	}
	l := compose.Compose{}
	ko, err := l.LoadFile([]string{path}, nil, false)
	if err != nil {
		t.Fatalf("unable to load compose file: %v", err)
	}
	opt.CreateD = true
	opt.InputFiles = []string{path}
	k := Kubernetes{Opt: opt}
	objs, err := k.Transform(ko, opt)
	if err != nil {
		t.Fatalf("unable to transform: %v", err)
	}
	return objs, ko
}

func pvcsOf(objs []runtime.Object) []*api.PersistentVolumeClaim {
	var pvcs []*api.PersistentVolumeClaim
	for _, obj := range objs {
		if pvc, ok := obj.(*api.PersistentVolumeClaim); ok {
			pvcs = append(pvcs, pvc)
		}
	}
	return pvcs
}

func deploymentOf(t *testing.T, objs []runtime.Object, name string) *appsv1.Deployment {
	t.Helper()
	for _, obj := range objs {
		if d, ok := obj.(*appsv1.Deployment); ok && d.Name == name {
			return d
		}
	}
	t.Fatalf("deployment %s not found in converted objects", name)
	return nil
}

// Baseline 1: a named volume lands on a PersistentVolumeClaim.
func TestVolumeMappingBaselineNamedVolume(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    volumes:
      - dbdata:/var/lib/mysql
volumes:
  dbdata:
`, kobject.ConvertOptions{})

	pvcs := pvcsOf(objs)
	if len(pvcs) != 1 || pvcs[0].Name != "dbdata" {
		t.Fatalf("expected exactly one PVC named dbdata, got %v", pvcs)
	}
	if len(pvcs[0].Spec.AccessModes) != 1 || pvcs[0].Spec.AccessModes[0] != api.ReadWriteOnce {
		t.Errorf("expected RWO access mode, got %v", pvcs[0].Spec.AccessModes)
	}
	if got := pvcs[0].Spec.Resources.Requests[api.ResourceStorage]; got.String() != PVCRequestSize {
		t.Errorf("expected default size %s, got %s", PVCRequestSize, got.String())
	}
	found := false
	for _, v := range deploymentOf(t, objs, "app").Spec.Template.Spec.Volumes {
		if v.Name == "dbdata" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "dbdata" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected pod volume backed by PVC dbdata")
	}
}

// Baseline 2: a bind mount lands on a hostPath volume, resolved against the
// compose file directory, and generates no PVC.
func TestVolumeMappingBaselineBindMount(t *testing.T) {
	dir := t.TempDir()
	objs, _ := convertCompose(t, dir, `
services:
  app:
    image: nginx
    volumes:
      - ./data:/data
`, kobject.ConvertOptions{})

	if pvcs := pvcsOf(objs); len(pvcs) != 0 {
		t.Fatalf("expected no PVC for a bind mount, got %v", pvcs)
	}
	expectedPath := filepath.Join(dir, "data")
	found := false
	for _, v := range deploymentOf(t, objs, "app").Spec.Template.Spec.Volumes {
		if v.HostPath != nil {
			found = true
			if v.HostPath.Path != expectedPath {
				t.Errorf("expected hostPath %s, got %s", expectedPath, v.HostPath.Path)
			}
		}
	}
	if !found {
		t.Errorf("expected a hostPath volume for the bind mount")
	}
}

// Baseline 3: tmpfs lands on an emptyDir with medium Memory.
func TestVolumeMappingBaselineTmpfs(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    tmpfs:
      - /run
`, kobject.ConvertOptions{})

	found := false
	for _, v := range deploymentOf(t, objs, "app").Spec.Template.Spec.Volumes {
		if v.EmptyDir != nil {
			found = true
			if v.EmptyDir.Medium != api.StorageMediumMemory {
				t.Errorf("expected Memory medium, got %q", v.EmptyDir.Medium)
			}
			if v.EmptyDir.SizeLimit != nil {
				t.Errorf("expected no size limit, got %v", v.EmptyDir.SizeLimit)
			}
		}
	}
	if !found {
		t.Errorf("expected an emptyDir volume for tmpfs")
	}
}

// The global --volumes option still forces every volume to the requested type,
// including downgrading bind mounts to emptyDir.
func TestVolumeMappingGlobalOptionForcesBindToEmptyDir(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    volumes:
      - ./data:/data
`, kobject.ConvertOptions{Volumes: "emptyDir"})

	for _, v := range deploymentOf(t, objs, "app").Spec.Template.Spec.Volumes {
		if v.HostPath != nil {
			t.Errorf("expected bind mount to be forced to emptyDir, got hostPath")
		}
		if v.EmptyDir != nil && v.EmptyDir.Medium == api.StorageMediumMemory {
			t.Errorf("expected plain emptyDir for a volume, got Memory medium")
		}
	}
	if pvcs := pvcsOf(objs); len(pvcs) != 0 {
		t.Fatalf("expected no PVC with --volumes emptyDir, got %v", pvcs)
	}
}

// The kompose.volume.type label overrides the global option for the service;
// invalid values remain an error.
func TestVolumeMappingLabelOverridesGlobalOption(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    labels:
      kompose.volume.type: hostPath
    volumes:
      - ./data:/data
`, kobject.ConvertOptions{Volumes: "emptyDir"})

	found := false
	for _, v := range deploymentOf(t, objs, "app").Spec.Template.Spec.Volumes {
		if v.HostPath != nil {
			found = true
		}
	}
	if !found {
		t.Errorf("expected kompose.volume.type label to override the global option")
	}
}

func TestVolumeMappingInvalidLabelTypeFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(path, []byte(`
services:
  app:
    image: nginx
    labels:
      kompose.volume.type: bogus
    volumes:
      - dbdata:/data
volumes:
  dbdata:
`), 0644); err != nil {
		t.Fatal(err)
	}
	l := compose.Compose{}
	ko, err := l.LoadFile([]string{path}, nil, false)
	if err != nil {
		t.Fatalf("unable to load compose file: %v", err)
	}
	k := Kubernetes{}
	if _, err := k.Transform(ko, kobject.ConvertOptions{CreateD: true}); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected invalid volume type error, got %v", err)
	}
}

// PVC size comes from three layers: the root-level volume label beats
// --pvc-request-size, which beats the built-in default.
func TestVolumeMappingSizePrecedence(t *testing.T) {
	composeWithSizeLabel := `
services:
  app:
    image: nginx
    volumes:
      - dbdata:/data
volumes:
  dbdata:
    labels:
      kompose.volume.size: 2Gi
`
	// label beats the command line option
	objs, _ := convertCompose(t, t.TempDir(), composeWithSizeLabel, kobject.ConvertOptions{PVCRequestSize: "1Gi"})
	if got := pvcsOf(objs)[0].Spec.Resources.Requests[api.ResourceStorage]; got.String() != "2Gi" {
		t.Errorf("expected label size 2Gi to win, got %s", got.String())
	}

	// command line option beats the built-in default
	composePlain := `
services:
  app:
    image: nginx
    volumes:
      - dbdata:/data
volumes:
  dbdata:
`
	objs, _ = convertCompose(t, t.TempDir(), composePlain, kobject.ConvertOptions{PVCRequestSize: "1Gi"})
	if got := pvcsOf(objs)[0].Spec.Resources.Requests[api.ResourceStorage]; got.String() != "1Gi" {
		t.Errorf("expected --pvc-request-size 1Gi, got %s", got.String())
	}

	// built-in default
	objs, _ = convertCompose(t, t.TempDir(), composePlain, kobject.ConvertOptions{})
	if got := pvcsOf(objs)[0].Spec.Resources.Requests[api.ResourceStorage]; got.String() != PVCRequestSize {
		t.Errorf("expected default %s, got %s", PVCRequestSize, got.String())
	}
}

// Sloppy sizes: surrounding whitespace is tolerated, an invalid suffix fails
// the whole conversion with the volume and service name in the message.
func TestVolumeMappingSizeParsing(t *testing.T) {
	composeWithSize := func(size string) string {
		return `
services:
  app:
    image: nginx
    volumes:
      - dbdata:/data
volumes:
  dbdata:
    labels:
      kompose.volume.size: "` + size + `"
`
	}

	objs, _ := convertCompose(t, t.TempDir(), composeWithSize("512Mi "), kobject.ConvertOptions{})
	if got := pvcsOf(objs)[0].Spec.Resources.Requests[api.ResourceStorage]; got.String() != "512Mi" {
		t.Errorf("expected 512Mi after trimming, got %s", got.String())
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(path, []byte(composeWithSize("1gi")), 0644); err != nil {
		t.Fatal(err)
	}
	l := compose.Compose{}
	ko, err := l.LoadFile([]string{path}, nil, false)
	if err != nil {
		t.Fatalf("unable to load compose file: %v", err)
	}
	k := Kubernetes{}
	_, err = k.Transform(ko, kobject.ConvertOptions{CreateD: true})
	if err == nil {
		t.Fatalf("expected error for invalid size 1gi")
	}
	if !strings.Contains(err.Error(), "dbdata") || !strings.Contains(err.Error(), "app") {
		t.Errorf("error should name the volume and the service, got: %v", err)
	}
}

// The kompose.volume.access-mode label sets the PVC access mode explicitly;
// the legacy :ro suffix keeps producing ReadOnlyMany for compatibility.
func TestVolumeMappingAccessModeLabel(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    volumes:
      - dbdata:/data
volumes:
  dbdata:
    labels:
      kompose.volume.access-mode: rwx
`, kobject.ConvertOptions{})
	if got := pvcsOf(objs)[0].Spec.AccessModes; len(got) != 1 || got[0] != api.ReadWriteMany {
		t.Errorf("expected RWX from label, got %v", got)
	}

	// legacy :ro suffix still produces ReadOnlyMany
	objs, _ = convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    volumes:
      - dbdata:/data:ro
volumes:
  dbdata:
`, kobject.ConvertOptions{})
	if got := pvcsOf(objs)[0].Spec.AccessModes; len(got) != 1 || got[0] != api.ReadOnlyMany {
		t.Errorf("expected ROX for compatibility, got %v", got)
	}
	// ...while the mount itself is read-only
	mounts := deploymentOf(t, objs, "app").Spec.Template.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 || !mounts[0].ReadOnly {
		t.Errorf("expected read-only volume mount, got %+v", mounts)
	}
}

func TestVolumeMappingInvalidAccessModeFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(path, []byte(`
services:
  app:
    image: nginx
    volumes:
      - dbdata:/data
volumes:
  dbdata:
    labels:
      kompose.volume.access-mode: readonly-please
`), 0644); err != nil {
		t.Fatal(err)
	}
	l := compose.Compose{}
	ko, err := l.LoadFile([]string{path}, nil, false)
	if err != nil {
		t.Fatalf("unable to load compose file: %v", err)
	}
	k := Kubernetes{}
	_, err = k.Transform(ko, kobject.ConvertOptions{CreateD: true})
	if err == nil || !strings.Contains(err.Error(), "dbdata") {
		t.Fatalf("expected access mode error naming the volume, got %v", err)
	}
}

// Two services sharing one named volume with different read-write needs get a
// single PVC generated as read-write (union), while the read-only service
// keeps its read-only mount.
func TestVolumeMappingSharedVolumeMerge(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  svc-a:
    image: nginx
    volumes:
      - dbdata:/a:ro
  svc-b:
    image: nginx
    volumes:
      - dbdata:/b
volumes:
  dbdata:
`, kobject.ConvertOptions{})

	pvcs := pvcsOf(objs)
	if len(pvcs) != 1 {
		t.Fatalf("expected a single shared PVC, got %d", len(pvcs))
	}
	if got := pvcs[0].Spec.AccessModes; len(got) != 1 || got[0] != api.ReadWriteOnce {
		t.Errorf("expected merged RWO, got %v", got)
	}
	for _, m := range deploymentOf(t, objs, "svc-a").Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "dbdata" && !m.ReadOnly {
			t.Errorf("expected svc-a to keep its read-only mount")
		}
	}
}

// tmpfs sizes from the long syntax (bytes) and the short option form (64m)
// become the emptyDir sizeLimit, using binary units (64m == 64Mi).
func TestVolumeMappingTmpfsSize(t *testing.T) {
	// long syntax: size in bytes
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    volumes:
      - type: tmpfs
        target: /run
        tmpfs:
          size: 67108864
`, kobject.ConvertOptions{})
	found := false
	for _, v := range deploymentOf(t, objs, "app").Spec.Template.Spec.Volumes {
		if v.EmptyDir != nil {
			found = true
			if v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.String() != "64Mi" {
				t.Errorf("expected sizeLimit 64Mi, got %v", v.EmptyDir.SizeLimit)
			}
		}
	}
	if !found {
		t.Fatalf("expected tmpfs to become an emptyDir volume")
	}

	// short option form: 64m means 64MiB (binary, per compose convention)
	objs, _ = convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    tmpfs:
      - /run:size=64m,mode=1777
`, kobject.ConvertOptions{})
	for _, v := range deploymentOf(t, objs, "app").Spec.Template.Spec.Volumes {
		if v.EmptyDir != nil {
			if v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.String() != "64Mi" {
				t.Errorf("expected sizeLimit 64Mi, got %v", v.EmptyDir.SizeLimit)
			}
		}
	}
}

// A read-only root filesystem must not make the tmpfs mount read-only: the
// emptyDir stays writable.
func TestVolumeMappingReadOnlyServiceKeepsTmpfsWritable(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    read_only: true
    tmpfs:
      - /run
`, kobject.ConvertOptions{})
	for _, m := range deploymentOf(t, objs, "app").Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/run" && m.ReadOnly {
			t.Errorf("tmpfs mount must stay writable under read_only")
		}
	}
}

// A bind mount of a single existing file keeps the established configMap
// conversion.
func TestVolumeMappingSingleFileBindStaysConfigMap(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(conf, []byte("key=value"), 0644); err != nil {
		t.Fatal(err)
	}
	objs, _ := convertCompose(t, dir, `
services:
  app:
    image: nginx
    volumes:
      - `+conf+`:/etc/app.conf
`, kobject.ConvertOptions{})
	foundCM := false
	for _, obj := range objs {
		if _, ok := obj.(*api.ConfigMap); ok {
			foundCM = true
		}
	}
	if !foundCM {
		t.Errorf("expected single-file bind mount to keep converting to a configMap")
	}
}

// external: true volumes reference an existing PVC and generate none.
func TestVolumeMappingExternalVolume(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    volumes:
      - dbdata:/data
volumes:
  dbdata:
    external: true
`, kobject.ConvertOptions{})
	if pvcs := pvcsOf(objs); len(pvcs) != 0 {
		t.Fatalf("expected no PVC for an external volume, got %v", pvcs)
	}
	found := false
	for _, v := range deploymentOf(t, objs, "app").Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "dbdata" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected pod to reference the existing PVC dbdata")
	}

	// an explicit name: is used as the claim name
	objs, _ = convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    volumes:
      - dbdata:/data
volumes:
  dbdata:
    external: true
    name: real-pvc
`, kobject.ConvertOptions{})
	found = false
	for _, v := range deploymentOf(t, objs, "app").Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "real-pvc" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected pod to reference the existing PVC real-pvc")
	}
}

// Service group mode must still produce a single PVC for a shared named volume.
func TestVolumeMappingServiceGroupMode(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  svc-a:
    image: nginx
    volumes:
      - dbdata:/a
  svc-b:
    image: nginx
    volumes:
      - dbdata:/b
volumes:
  dbdata:
`, kobject.ConvertOptions{ServiceGroupMode: "volume"})
	if pvcs := pvcsOf(objs); len(pvcs) != 1 || pvcs[0].Name != "dbdata" {
		t.Fatalf("expected a single PVC dbdata under service group mode, got %v", pvcs)
	}
}

// kompose.service.name_override renames the service before volumes are
// parsed, so generated PVC names follow the overridden name.
func TestVolumeMappingNameOverride(t *testing.T) {
	objs, _ := convertCompose(t, t.TempDir(), `
services:
  app:
    image: nginx
    labels:
      kompose.service.name_override: renamed
    volumes:
      - /data
`, kobject.ConvertOptions{})
	pvcs := pvcsOf(objs)
	if len(pvcs) != 1 || pvcs[0].Name != "renamed-claim0" {
		t.Fatalf("expected PVC renamed-claim0, got %v", pvcs)
	}
}

// Converting the same input twice yields byte-identical output, and PVCs are
// ordered by name.
func TestVolumeMappingDeterministicOutput(t *testing.T) {
	composeFile := `
services:
  app:
    image: nginx
    volumes:
      - zdata:/z
      - adata:/a
volumes:
  zdata:
  adata:
`
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(path, []byte(composeFile), 0644); err != nil {
		t.Fatal(err)
	}
	render := func() string {
		l := compose.Compose{}
		ko, err := l.LoadFile([]string{path}, nil, false)
		if err != nil {
			t.Fatalf("unable to load compose file: %v", err)
		}
		k := Kubernetes{}
		objs, err := k.Transform(ko, kobject.ConvertOptions{CreateD: true})
		if err != nil {
			t.Fatalf("unable to transform: %v", err)
		}
		var sb strings.Builder
		for _, obj := range objs {
			data, err := json.Marshal(obj)
			if err != nil {
				t.Fatalf("unable to marshal object: %v", err)
			}
			sb.Write(data)
			sb.WriteString("\n")
		}
		return sb.String()
	}
	first, second := render(), render()
	if first != second {
		t.Errorf("expected byte-identical output across conversions")
	}

	// PVCs are sorted by name in the output
	l := compose.Compose{}
	ko, _ := l.LoadFile([]string{path}, nil, false)
	k := Kubernetes{}
	objs, _ := k.Transform(ko, kobject.ConvertOptions{CreateD: true})
	pvcs := pvcsOf(objs)
	if len(pvcs) != 2 || pvcs[0].Name != "adata" || pvcs[1].Name != "zdata" {
		t.Errorf("expected PVCs sorted by name [adata zdata], got %v", pvcs)
	}
}

// tmpfs sizes: bare numbers are bytes, k/m/g suffixes are binary (64m == 64Mi),
// invalid values yield no limit (and a warning) instead of failing conversion.
func TestParseTmpfsSize(t *testing.T) {
	cases := map[string]string{
		"67108864": "64Mi",
		"64m":      "64Mi",
		"64M":      "64Mi",
		"64mb":     "64Mi",
		"512k":     "512Ki",
		"1g":       "1Gi",
		"1024":     "1Ki",
	}
	for in, want := range cases {
		q, err := parseTmpfsSize(in)
		if err != nil {
			t.Errorf("parseTmpfsSize(%q) returned error: %v", in, err)
			continue
		}
		if q.String() != want {
			t.Errorf("parseTmpfsSize(%q) = %s, expected %s", in, q.String(), want)
		}
	}
	for _, in := range []string{"", "abc", "m", "64x", "64mbb"} {
		if q, err := parseTmpfsSize(in); err == nil {
			t.Errorf("parseTmpfsSize(%q) expected error, got %v", in, q)
		}
	}
}
