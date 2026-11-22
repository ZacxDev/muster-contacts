// Package deploy pins what the deployment files promise: the Kubernetes
// base as `kubectl kustomize` renders it, and the example overlay. A test
// reads the RENDERED objects, never the files' wording.
//
// kubectl is needed to render; without it the tests skip, unless
// CONTACTS_REQUIRE_KUBECTL=1 (CI and scripts/mutate set it), which makes
// them fail instead, so a sweep can never pass by skipping.
package deploy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// render is `kubectl kustomize dir`, decoded.
func render(t *testing.T, dir string) []obj {
	t.Helper()
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		if os.Getenv("CONTACTS_REQUIRE_KUBECTL") == "1" {
			t.Fatal("kubectl is not on PATH and CONTACTS_REQUIRE_KUBECTL=1")
		}
		t.Skip("kubectl is not on PATH (CONTACTS_REQUIRE_KUBECTL=1 makes this fail)")
	}
	var stderr bytes.Buffer
	cmd := exec.Command(kubectl, "kustomize", dir)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v\n%s", dir, err, stderr.String())
	}
	var docs []obj
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var o obj
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decoding %s: %v", dir, err)
		}
		if o != nil {
			docs = append(docs, o)
		}
	}
	return docs
}

type obj = map[string]any

// dig walks m by keys (string) and indexes (int); nil when absent.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(obj)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				return nil
			}
			v = s[k]
		}
	}
	return v
}

func find(t *testing.T, docs []obj, kind, name string) obj {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == kind && dig(d, "metadata", "name") == name {
			return d
		}
	}
	t.Fatalf("no %s %q rendered", kind, name)
	return nil
}

func ofKind(docs []obj, kind string) []obj {
	var out []obj
	for _, d := range docs {
		if d["kind"] == kind {
			out = append(out, d)
		}
	}
	return out
}

func container(t *testing.T, dep obj, name string) obj {
	t.Helper()
	cs, _ := dig(dep, "spec", "template", "spec", "containers").([]any)
	for _, c := range cs {
		if dig(c, "name") == name {
			return c.(obj)
		}
	}
	t.Fatalf("deployment %v has no container %q", dig(dep, "metadata", "name"), name)
	return nil
}

// envEntry is one container env entry and its position.
func envEntry(c obj, name string) (obj, int) {
	es, _ := c["env"].([]any)
	for i, e := range es {
		if dig(e, "name") == name {
			return e.(obj), i
		}
	}
	return nil, -1
}

var dirs = []string{"kubernetes", "example-overlay"}

// The plugin's Deployment: one replica, Recreate (one SQLite writer),
// nonroot, read-only root, every capability dropped, no service account
// token; both secrets from the Secret, never a literal; the state on its
// PVC; the probes on the internal listener.
func TestKubernetesPlugin(t *testing.T) {
	for _, dir := range dirs {
		docs := render(t, dir)
		dep := find(t, docs, "Deployment", "muster-contacts")
		if dig(dep, "spec", "replicas") != 1 || dig(dep, "spec", "strategy", "type") != "Recreate" {
			t.Errorf("%s: replicas %v, strategy %v: want 1 and Recreate", dir, dig(dep, "spec", "replicas"), dig(dep, "spec", "strategy", "type"))
		}
		pod := dig(dep, "spec", "template", "spec")
		if dig(pod, "automountServiceAccountToken") != false || dig(pod, "securityContext", "runAsNonRoot") != true {
			t.Errorf("%s: the pod mounts a token or may run as root", dir)
		}
		c := container(t, dep, "muster-contacts")
		if dig(c, "securityContext", "readOnlyRootFilesystem") != true || dig(c, "securityContext", "allowPrivilegeEscalation") != false ||
			!reflect.DeepEqual(dig(c, "securityContext", "capabilities", "drop"), []any{"ALL"}) {
			t.Errorf("%s: container securityContext %v", dir, dig(c, "securityContext"))
		}
		for _, name := range []string{"MUSTER_PLUGIN_TOKEN", "CONTACTS_API_TOKEN"} {
			e, _ := envEntry(c, name)
			if e == nil || dig(e, "value") != nil || dig(e, "valueFrom", "secretKeyRef", "name") != "muster-contacts" || dig(e, "valueFrom", "secretKeyRef", "key") != name {
				t.Errorf("%s: %s = %v, want it from the muster-contacts Secret", dir, name, e)
			}
		}
		if e, _ := envEntry(c, "STATE_DIR"); dig(e, "value") != "/state" {
			t.Errorf("%s: STATE_DIR %v", dir, e)
		}
		vols := map[string]any{}
		for _, v := range dig(pod, "volumes").([]any) {
			vols[dig(v, "name").(string)] = v
		}
		if dig(vols["state"], "persistentVolumeClaim", "claimName") != "muster-contacts-state" || dig(vols["tmp"], "emptyDir") == nil {
			t.Errorf("%s: volumes %v", dir, vols)
		}
		for _, p := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
			if dig(c, p, "httpGet", "path") != "/healthz" || dig(c, p, "httpGet", "port") != "internal" {
				t.Errorf("%s: %s %v, want /healthz on the internal port", dir, p, dig(c, p))
			}
		}
		svc := find(t, docs, "Service", "muster-contacts")
		if dig(svc, "spec", "type") != "ClusterIP" || dig(svc, "spec", "ports", 0, "port") != 8080 || dig(svc, "spec", "ports", 1, "port") != 8081 {
			t.Errorf("%s: service %v", dir, dig(svc, "spec"))
		}
	}
}

// 🔴 The network policy: the API port (:8081) is reachable from the
// meetings plugin's pods ALONE; the public port from the ingress
// controller's namespace alone; out, only muster and DNS.
func TestKubernetesNetworkPolicy(t *testing.T) {
	for _, dir := range dirs {
		np := find(t, render(t, dir), "NetworkPolicy", "muster-contacts")
		if !reflect.DeepEqual(dig(np, "spec", "policyTypes"), []any{"Ingress", "Egress"}) {
			t.Errorf("%s: policyTypes %v, want both (default-deny)", dir, dig(np, "spec", "policyTypes"))
		}
		in, _ := dig(np, "spec", "ingress").([]any)
		if len(in) != 2 {
			t.Fatalf("%s: %d ingress rules, want 2", dir, len(in))
		}
		byPort := map[int]any{}
		for _, r := range in {
			ports, _ := dig(r, "ports").([]any)
			from, _ := dig(r, "from").([]any)
			if len(ports) != 1 || len(from) != 1 {
				t.Errorf("%s: an ingress rule %v: want one port from one peer", dir, r)
				continue
			}
			byPort[dig(ports[0], "port").(int)] = from[0]
		}
		api := byPort[8081]
		if api == nil || dig(api, "podSelector", "matchLabels", "app.kubernetes.io/name") != "muster-meetings" || dig(api, "namespaceSelector") != nil || dig(api, "ipBlock") != nil {
			t.Errorf("%s: the API port :8081 admits %v, want the meetings plugin's pods alone", dir, api)
		}
		pub := byPort[8080]
		if pub == nil || dig(pub, "namespaceSelector") == nil || dig(pub, "podSelector") != nil {
			t.Errorf("%s: the public port :8080 admits %v, want the ingress controller's namespace", dir, pub)
		}
		out, _ := dig(np, "spec", "egress").([]any)
		if len(out) != 2 || dig(out[0], "ports", 0, "port") != 8105 || dig(out[1], "ports", 0, "port") != 53 {
			t.Errorf("%s: egress %v, want muster's API and DNS only", dir, out)
		}
	}
}

// Every key of the params ConfigMap is read by the Deployment, and every
// key the Deployment reads exists.
func TestKubernetesParamsAreAllUsed(t *testing.T) {
	docs := render(t, "kubernetes")
	var cm obj
	for _, d := range ofKind(docs, "ConfigMap") {
		if strings.HasPrefix(dig(d, "metadata", "name").(string), "muster-contacts-params") {
			cm = d
		}
	}
	if cm == nil {
		t.Fatal("no params ConfigMap")
	}
	used := map[string]bool{}
	c := container(t, find(t, docs, "Deployment", "muster-contacts"), "muster-contacts")
	for _, e := range c["env"].([]any) {
		if k, ok := dig(e, "valueFrom", "configMapKeyRef", "key").(string); ok {
			used[k] = true
			if _, ok := dig(cm, "data").(obj)[k]; !ok {
				t.Errorf("the Deployment reads %s, which the ConfigMap does not hold", k)
			}
		}
	}
	for k := range dig(cm, "data").(obj) {
		if !used[k] {
			t.Errorf("params key %s is read by nothing", k)
		}
	}
}

// The example overlay sets every knob: the namespace, a pinned image, every
// param, the StorageClass and the policy's selectors.
func TestTheExampleOverlaySetsEveryKnob(t *testing.T) {
	docs := render(t, "example-overlay")
	for _, d := range docs {
		if ns := dig(d, "metadata", "namespace"); ns != "muster-tenant" {
			t.Errorf("%v %v: namespace %v", d["kind"], dig(d, "metadata", "name"), ns)
		}
	}
	c := container(t, find(t, docs, "Deployment", "muster-contacts"), "muster-contacts")
	if img := dig(c, "image").(string); strings.HasSuffix(img, ":main") {
		t.Errorf("the overlay runs %s: pin a sha-<commit> tag or a digest", img)
	}
	if dig(find(t, docs, "PersistentVolumeClaim", "muster-contacts-state"), "spec", "storageClassName") == nil {
		t.Error("no StorageClass")
	}
	np := find(t, docs, "NetworkPolicy", "muster-contacts")
	if dig(np, "spec", "ingress", 0, "from", 0, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != "example-ingress" {
		t.Error("the ingress namespace is not patched")
	}
	for _, cm := range ofKind(docs, "ConfigMap") {
		if v := dig(cm, "data", "meetings-app-id"); v == "" {
			t.Error("the overlay leaves meetings-app-id empty")
		}
	}
}

// The public repository names no real host: every host in the rendered
// files is example.test, a cluster Service name, or the image registry.
func TestTheDeploymentFilesNameNoRealHost(t *testing.T) {
	host := regexp.MustCompile(`[a-z0-9.-]+\.(?:[a-z]{2,})(?::\d+)?`)
	allowed := regexp.MustCompile(`(?:^|\.)example\.test$|\.svc\.cluster\.local\.?$|^ghcr\.io$|^kubernetes\.io$|^app\.kubernetes\.io$|^networking\.k8s\.io$|^metadata\.name$|^cluster\.local$|^k8s\.io$`)
	for _, dir := range dirs {
		kubectl, err := exec.LookPath("kubectl")
		if err != nil {
			render(t, dir) // skips or fails as render does
		}
		out, err := exec.Command(kubectl, "kustomize", dir).Output()
		if err != nil {
			t.Fatal(err)
		}
		found := host.FindAllString(string(out), -1)
		// The positive control: the scan sees the example host it allows.
		if !slices.Contains(found, "contacts-muster.example.test") {
			t.Fatalf("%s: the host scan did not see contacts-muster.example.test (found %v)", dir, found)
		}
		for _, h := range found {
			h = strings.Split(h, ":")[0]
			if !allowed.MatchString(h) && !strings.Contains(h, "kubernetes.io") && !strings.HasPrefix(h, "muster-contacts") {
				t.Errorf("%s: a host %q", dir, h)
			}
		}
	}
}
