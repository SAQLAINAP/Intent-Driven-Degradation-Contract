// Package k8s contains the Kubernetes reconciler for tier-driven replica management.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// k8sClient is a minimal Kubernetes REST API client backed by standard HTTP.
// It avoids the client-go dependency while supporting both in-cluster and kubeconfig auth.
type k8sClient struct {
	server string
	token  string
	hc     *http.Client
}

// Deployment is a minimal appsv1.Deployment view containing only IDDC-relevant fields.
type Deployment struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Replicas int32 `json:"replicas"`
		Paused   bool  `json:"paused"`
	} `json:"spec"`
}

// hpa is a minimal autoscaling/v2 HorizontalPodAutoscaler.
type hpa struct {
	Spec struct {
		MinReplicas *int32 `json:"minReplicas"`
		MaxReplicas int32  `json:"maxReplicas"`
	} `json:"spec"`
}

// Node is a minimal v1.Node.
type Node struct {
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
}

type nodeList struct {
	Items []Node `json:"items"`
}

// newK8sClient builds a client from the environment.
// Priority: in-cluster service account → kubeconfigPath arg → KUBECONFIG env → ~/.kube/config.
func newK8sClient(kubeconfigPath string) (*k8sClient, error) {
	// In-cluster: service account token is mounted at a well-known path.
	if token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		host := os.Getenv("KUBERNETES_SERVICE_HOST")
		port := os.Getenv("KUBERNETES_SERVICE_PORT")
		if port == "" {
			port = "443"
		}
		if host != "" {
			tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
			if ca, err2 := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"); err2 == nil {
				pool := x509.NewCertPool()
				pool.AppendCertsFromPEM(ca)
				tlsCfg.RootCAs = pool
			}
			return &k8sClient{
				server: fmt.Sprintf("https://%s:%s", host, port),
				token:  string(token),
				hc:     &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
			}, nil
		}
	}

	// Out-of-cluster: resolve kubeconfig path.
	if kubeconfigPath == "" {
		kubeconfigPath = os.Getenv("KUBECONFIG")
	}
	if kubeconfigPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			kubeconfigPath = filepath.Join(home, ".kube", "config")
		}
	}
	return parseKubeconfig(kubeconfigPath)
}

// minimalKubeconfig holds only what we need from ~/.kube/config.
type minimalKubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
		} `yaml:"user"`
	} `yaml:"users"`
}

func parseKubeconfig(path string) (*k8sClient, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig %s: %w", path, err)
	}
	var kc minimalKubeconfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig: %w", err)
	}

	var clusterName, userName string
	for _, c := range kc.Contexts {
		if c.Name == kc.CurrentContext {
			clusterName = c.Context.Cluster
			userName = c.Context.User
			break
		}
	}

	var server string
	var caData []byte
	var skipTLS bool
	for _, cl := range kc.Clusters {
		if cl.Name == clusterName {
			server = cl.Cluster.Server
			skipTLS = cl.Cluster.InsecureSkipTLSVerify
			if d := cl.Cluster.CertificateAuthorityData; d != "" {
				caData, _ = base64.StdEncoding.DecodeString(d)
			}
			break
		}
	}
	if server == "" {
		return nil, fmt.Errorf("no server for cluster %q in kubeconfig %s", clusterName, path)
	}

	var token string
	var certData, keyData []byte
	for _, u := range kc.Users {
		if u.Name == userName {
			token = u.User.Token
			if d := u.User.ClientCertificateData; d != "" {
				certData, _ = base64.StdEncoding.DecodeString(d)
			}
			if d := u.User.ClientKeyData; d != "" {
				keyData, _ = base64.StdEncoding.DecodeString(d)
			}
			break
		}
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if skipTLS {
		tlsCfg.InsecureSkipVerify = true // #nosec G402 -- explicit user opt-in via kubeconfig
	} else if len(caData) > 0 {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caData)
		tlsCfg.RootCAs = pool
	}
	if len(certData) > 0 && len(keyData) > 0 {
		cert, err := tls.X509KeyPair(certData, keyData)
		if err == nil {
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
	}

	return &k8sClient{
		server: server,
		token:  token,
		hc:     &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	}, nil
}

// do performs an authenticated request against the K8s API server and JSON-decodes the response.
func (c *k8sClient) do(ctx context.Context, method, path, contentType string, body, out interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.server+path, bodyReader)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		ct := contentType
		if ct == "" {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("k8s %s %s → HTTP %d: %s", method, path, resp.StatusCode, respBytes)
	}
	if out != nil {
		return json.Unmarshal(respBytes, out)
	}
	return nil
}

// ─── Deployment helpers ───────────────────────────────────────────────────────

func (c *k8sClient) GetDeployment(ctx context.Context, ns, name string) (*Deployment, error) {
	var d Deployment
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, name),
		"", nil, &d)
	return &d, err
}

func (c *k8sClient) PatchDeploymentReplicas(ctx context.Context, ns, name string, replicas int32) error {
	body := map[string]interface{}{"spec": map[string]interface{}{"replicas": replicas}}
	return c.do(ctx, http.MethodPatch,
		fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, name),
		"application/merge-patch+json", body, nil)
}

func (c *k8sClient) PatchDeploymentPaused(ctx context.Context, ns, name string, paused bool) error {
	body := map[string]interface{}{"spec": map[string]interface{}{"paused": paused}}
	return c.do(ctx, http.MethodPatch,
		fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, name),
		"application/merge-patch+json", body, nil)
}

func (c *k8sClient) AnnotateDeployment(ctx context.Context, ns, name, key, value string) error {
	var v interface{} = value
	if value == "" {
		v = nil // null in JSON removes the annotation
	}
	body := map[string]interface{}{
		"metadata": map[string]interface{}{"annotations": map[string]interface{}{key: v}},
	}
	return c.do(ctx, http.MethodPatch,
		fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", ns, name),
		"application/merge-patch+json", body, nil)
}

// ─── HPA helpers ──────────────────────────────────────────────────────────────

func (c *k8sClient) GetHPA(ctx context.Context, ns, name string) (*hpa, error) {
	var h hpa
	err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/apis/autoscaling/v2/namespaces/%s/horizontalpodautoscalers/%s", ns, name),
		"", nil, &h)
	return &h, err
}

func (c *k8sClient) PatchHPABounds(ctx context.Context, ns, name string, min, max int32) error {
	body := map[string]interface{}{
		"spec": map[string]interface{}{
			"minReplicas": min,
			"maxReplicas": max,
		},
	}
	return c.do(ctx, http.MethodPatch,
		fmt.Sprintf("/apis/autoscaling/v2/namespaces/%s/horizontalpodautoscalers/%s", ns, name),
		"application/merge-patch+json", body, nil)
}

// ─── Node helpers ─────────────────────────────────────────────────────────────

func (c *k8sClient) ListNodes(ctx context.Context) ([]Node, error) {
	var nl nodeList
	if err := c.do(ctx, http.MethodGet, "/api/v1/nodes", "", nil, &nl); err != nil {
		return nil, err
	}
	return nl.Items, nil
}

func (c *k8sClient) SetNodeUnschedulable(ctx context.Context, name string, val bool) error {
	body := map[string]interface{}{"spec": map[string]interface{}{"unschedulable": val}}
	return c.do(ctx, http.MethodPatch,
		fmt.Sprintf("/api/v1/nodes/%s", name),
		"application/merge-patch+json", body, nil)
}
