// Package kube is the few Kubernetes API calls the node plugin makes, over
// net/http with the pod's service-account token. It avoids client-go to
// keep the dependency tree small.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	saDir     = "/var/run/secrets/kubernetes.io/serviceaccount"
	userAgent = "fileblock-node"
)

type Client interface {
	ListPodsOnNode(ctx context.Context, node string) ([]Pod, error)
	DeletePod(ctx context.Context, namespace, name, uid string) error
	PodEvent(ctx context.Context, pod Pod, reason, message string) error
	PolicyEnforced(ctx context.Context, name string) (bool, error)
}

type HTTPClient struct {
	base      string
	hc        *http.Client
	tokenPath string
}

func NewHTTPClient(baseURL string, hc *http.Client, tokenPath string) *HTTPClient {
	return &HTTPClient{base: strings.TrimRight(baseURL, "/"), hc: hc, tokenPath: tokenPath}
}

// InCluster builds a client from the pod's service-account mount and the
// KUBERNETES_SERVICE_* env the kubelet injects.
func InCluster() (*HTTPClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a cluster: KUBERNETES_SERVICE_HOST/PORT unset")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificates in %s/ca.crt", saDir)
	}
	hc := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	return NewHTTPClient("https://"+net.JoinHostPort(host, port), hc, saDir+"/token"), nil
}

type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("kube API %d: %s", e.Code, e.Message) }

func (c *HTTPClient) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return fmt.Errorf("read service-account token: %w", err)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &st)
		if st.Message == "" {
			st.Message = http.StatusText(resp.StatusCode)
		}
		return &apiError{Code: resp.StatusCode, Message: st.Message}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func isNotFound(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Code == http.StatusNotFound
}

func (c *HTTPClient) ListPodsOnNode(ctx context.Context, node string) ([]Pod, error) {
	var list struct {
		Items []Pod `json:"items"`
	}
	q := url.Values{"fieldSelector": {"spec.nodeName=" + node}}
	if err := c.do(ctx, http.MethodGet, "/api/v1/pods?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c *HTTPClient) DeletePod(ctx context.Context, namespace, name, uid string) error {
	body := map[string]any{
		"apiVersion":    "v1",
		"kind":          "DeleteOptions",
		"preconditions": map[string]string{"uid": uid},
	}
	err := c.do(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+url.PathEscape(namespace)+"/pods/"+url.PathEscape(name), body, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (c *HTTPClient) PodEvent(ctx context.Context, pod Pod, reason, message string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	ev := map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]string{"generateName": pod.Metadata.Name + ".", "namespace": pod.Metadata.Namespace},
		"involvedObject": map[string]string{
			"apiVersion": "v1", "kind": "Pod",
			"name": pod.Metadata.Name, "namespace": pod.Metadata.Namespace, "uid": pod.Metadata.UID,
		},
		"reason":         reason,
		"message":        message,
		"type":           "Warning",
		"source":         map[string]string{"component": "fileblock-node", "host": pod.Spec.NodeName},
		"firstTimestamp": now,
		"lastTimestamp":  now,
		"count":          1,
	}
	return c.do(ctx, http.MethodPost, "/api/v1/namespaces/"+url.PathEscape(pod.Metadata.Namespace)+"/events", ev, nil)
}

func (c *HTTPClient) PolicyEnforced(ctx context.Context, name string) (bool, error) {
	const base = "/apis/admissionregistration.k8s.io/v1/"
	if err := c.do(ctx, http.MethodGet, base+"validatingadmissionpolicies/"+url.PathEscape(name), nil, nil); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	var b struct {
		Spec struct {
			ValidationActions []string `json:"validationActions"`
		} `json:"spec"`
	}
	if err := c.do(ctx, http.MethodGet, base+"validatingadmissionpolicybindings/"+url.PathEscape(name), nil, &b); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return slices.Contains(b.Spec.ValidationActions, "Deny"), nil
}
