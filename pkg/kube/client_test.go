package kube

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *HTTPClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("tok-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewHTTPClient(srv.URL, srv.Client(), tok)
}

func TestListPodsOnNode(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pods" || r.URL.Query().Get("fieldSelector") != "spec.nodeName=node-a" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"items":[{"metadata":{"name":"p","namespace":"ns","uid":"u1",
		  "ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"rs","controller":true}]},
		  "spec":{"nodeName":"node-a"},"status":{"startTime":"2026-01-01T00:00:00Z"}}]}`)
	})
	pods, err := c.ListPodsOnNode(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Metadata.UID != "u1" || pods[0].ControllerOwner() == nil || pods[0].Status.StartTime == nil {
		t.Fatalf("decoded %+v", pods)
	}
}

func TestDeletePodSendsUIDPrecondition(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/namespaces/ns/pods/p" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		pre, _ := body["preconditions"].(map[string]any)
		if pre["uid"] != "u1" {
			t.Errorf("preconditions: %v", body)
		}
		if _, ok := body["gracePeriodSeconds"]; ok {
			t.Errorf("must not override the pod's grace period: %v", body)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{}`)
	})
	if err := c.DeletePod(context.Background(), "ns", "p", "u1"); err != nil {
		t.Fatal(err)
	}
}

func TestDeletePodNotFoundIsNil(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := c.DeletePod(context.Background(), "ns", "p", "u1"); err != nil {
		t.Fatalf("NotFound should be nil, got %v", err)
	}
}

func TestDeletePodForbiddenIsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"denied by policy"}`)
	})
	err := c.DeletePod(context.Background(), "ns", "p", "u1")
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("want the API message in the error, got %v", err)
	}
}

func TestPodEvent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/ns/events" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		var ev map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ev)
		if ev["reason"] != "FileblockVolumeFailed" || ev["type"] != "Warning" {
			t.Errorf("event: %v", ev)
		}
		io_ := ev["involvedObject"].(map[string]any)
		if io_["uid"] != "u1" || io_["kind"] != "Pod" {
			t.Errorf("involvedObject: %v", io_)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{}`)
	})
	var p Pod
	p.Metadata.Name, p.Metadata.Namespace, p.Metadata.UID = "p", "ns", "u1"
	if err := c.PodEvent(context.Background(), p, "FileblockVolumeFailed", "msg"); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyEnforced(t *testing.T) {
	for name, tc := range map[string]struct {
		policy, binding int
		actions         string
		want            bool
	}{
		"both with Deny":  {200, 200, `["Deny"]`, true},
		"audit only":      {200, 200, `["Audit"]`, false},
		"missing policy":  {404, 200, `["Deny"]`, false},
		"missing binding": {200, 404, `["Deny"]`, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies/fileblock-node-pod-delete":
					w.WriteHeader(tc.policy)
					io.WriteString(w, `{}`)
				case "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/fileblock-node-pod-delete":
					w.WriteHeader(tc.binding)
					io.WriteString(w, `{"spec":{"validationActions":`+tc.actions+`}}`)
				default:
					t.Errorf("unexpected %s", r.URL)
				}
			})
			got, err := c.PolicyEnforced(context.Background(), "fileblock-node-pod-delete")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("PolicyEnforced=%v want %v", got, tc.want)
			}
		})
	}
}

func TestTokenReReadEachRequest(t *testing.T) {
	var seen []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		io.WriteString(w, `{"items":[]}`)
	})
	_, _ = c.ListPodsOnNode(context.Background(), "n")
	_ = os.WriteFile(c.tokenPath, []byte("tok-2"), 0o600)
	_, _ = c.ListPodsOnNode(context.Background(), "n")
	if len(seen) != 2 || seen[1] != "Bearer tok-2" {
		t.Fatalf("token not re-read: %v", seen)
	}
}
