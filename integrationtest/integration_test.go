package integrationtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/vault/api"
)

// Set the environment variable INTEGRATION_TESTS to any non-empty value to run
// the tests in this package. The test assumes it has available:
//   - kubectl
//   - A Kubernetes cluster in which:
//       - it can use the `test` namespace
//       - Vault is deployed and accessible
//       - There is a serviceaccount called test-token-reviewer-account with access to the TokenReview API
// See `make setup-integration-test` for manual testing.
func TestMain(m *testing.M) {
	if os.Getenv("INTEGRATION_TESTS") != "" {
		checkKubectlVersion()
		os.Setenv("VAULT_ADDR", "http://127.0.0.1:38200")
		os.Setenv("VAULT_TOKEN", "root")
		os.Setenv("KUBERNETES_JWT", getVaultServiceAccountJWT())
		os.Setenv("TOKEN_REVIEWER_JWT", getTokenReviewerJWT())
		os.Exit(m.Run())
	}
}

type kubectlVersion struct {
	ClientVersion struct {
		GitVersion string `json:"gitVersion"`
	} `json:"clientVersion"`
}

// kubectl create token requires kubectl >= v1.24.0
func checkKubectlVersion() {
	versionJSON := runCmd("kubectl version --client --output=json")
	var versionInfo kubectlVersion

	if err := json.Unmarshal([]byte(versionJSON), &versionInfo); err != nil {
		panic(err)
	}

	v := version.Must(version.NewSemver(versionInfo.ClientVersion.GitVersion))
	if v.LessThan(version.Must(version.NewSemver("v1.24.0"))) {
		panic("integration tests require kubectl version >= v1.24.0, but found: " + versionInfo.ClientVersion.GitVersion)
	}
}

func getTokenReviewerJWT() string {
	return runCmd("kubectl --namespace=test create token test-token-reviewer-account")
}

func getVaultServiceAccountJWT() string {
	return runCmd("kubectl --namespace=test create token vault")
}

// runCmd returns standard out + standard error
func runCmd(command string) string {
	parts := strings.Split(command, " ")
	cmd := exec.Command(parts[0], parts[1:]...)
	out := &bytes.Buffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		panic(fmt.Sprintf("Got unexpected output: %s, err = %s", out.String(), err))
	}
	return out.String()
}

func setupKubernetesAuth(t *testing.T, boundServiceAccountName string, kubeConfigOverride map[string]interface{}) (*api.Client, func()) {
	t.Helper()
	// Pick up VAULT_ADDR and VAULT_TOKEN from env vars
	client, err := api.NewClient(nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Write("sys/auth/kubernetes", map[string]interface{}{
		"type": "kubernetes-dev",
	})
	if err != nil {
		t.Fatal(err)
	}

	deferred := func() {
		_, err = client.Logical().Delete("sys/auth/kubernetes")
		if err != nil {
			t.Fatal(err)
		}
	}

	defer func() {
		// just in case setupKubernetesAuth panics before returning deferred to the caller
		if panicErr := recover(); panicErr != nil {
			deferred()
			panic(panicErr)
		} else if t.Failed() {
			deferred()
		}
	}()

	if len(kubeConfigOverride) == 0 {
		_, err = client.Logical().Write("auth/kubernetes/config", map[string]interface{}{
			"kubernetes_host": "https://kubernetes.default.svc.cluster.local",
		})
	} else {
		_, err = client.Logical().Write("auth/kubernetes/config", kubeConfigOverride)
	}
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Write("auth/kubernetes/role/test-role", map[string]interface{}{
		"bound_service_account_names":      boundServiceAccountName,
		"bound_service_account_namespaces": "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	return client, deferred
}

func TestSuccess(t *testing.T) {
	client, deferred := setupKubernetesAuth(t, "vault", nil)
	defer deferred()

	_, err := client.Logical().Write("auth/kubernetes/login", map[string]interface{}{
		"role": "test-role",
		"jwt":  os.Getenv("KUBERNETES_JWT"),
	})
	if err != nil {
		t.Fatalf("Expected successful login but got: %v", err)
	}
}

func TestSuccessWithTokenReviewerJwt(t *testing.T) {
	client, deferred := setupKubernetesAuth(t, "vault", map[string]interface{}{
		"kubernetes_host":    "https://kubernetes.default.svc.cluster.local",
		"token_reviewer_jwt": os.Getenv("TOKEN_REVIEWER_JWT"),
	})
	defer deferred()

	_, err := client.Logical().Write("auth/kubernetes/login", map[string]interface{}{
		"role": "test-role",
		"jwt":  os.Getenv("KUBERNETES_JWT"),
	})
	if err != nil {
		t.Fatalf("Expected successful login but got: %v", err)
	}
}

func TestFailWithBadTokenReviewerJwt(t *testing.T) {
	client, deferred := setupKubernetesAuth(t, "vault", map[string]interface{}{
		"kubernetes_host":    "https://kubernetes.default.svc.cluster.local",
		"token_reviewer_jwt": badTokenReviewerJwt,
	})
	defer deferred()

	_, err := client.Logical().Write("auth/kubernetes/login", map[string]interface{}{
		"role": "test-role",
		"jwt":  os.Getenv("KUBERNETES_JWT"),
	})
	respErr, ok := err.(*api.ResponseError)
	if !ok {
		t.Fatalf("Expected api.ResponseError but was: %s", reflect.TypeOf(err).Name())
	}
	if respErr.StatusCode != http.StatusForbidden {
		t.Fatalf("Expected 403 but was %d: %s", respErr.StatusCode, respErr.Error())
	}
}

func TestUnauthorizedServiceAccountErrorCode(t *testing.T) {
	client, deferred := setupKubernetesAuth(t, "badServiceAccount", nil)
	defer deferred()

	_, err := client.Logical().Write("auth/kubernetes/login", map[string]interface{}{
		"role": "test-role",
		"jwt":  os.Getenv("KUBERNETES_JWT"),
	})
	respErr, ok := err.(*api.ResponseError)
	if !ok {
		t.Fatalf("Expected api.ResponseError but was: %s", reflect.TypeOf(err).Name())
	}
	if respErr.StatusCode != http.StatusForbidden {
		t.Fatalf("Expected 403 but was %d: %s", respErr.StatusCode, respErr.Error())
	}
}

var badTokenReviewerJwt = "eyJhbGciOiJSUzI1NiIsImtpZCI6IkZza1ViNWREek8tQ05uaVk3TU5mRWZ2dEx5bzFuU0tsV3JhUU5nekhVQ28ifQ.eyJhdWQiOlsiaHR0cHM6Ly9rdWJlcm5ldGVzLmRlZmF1bHQuc3ZjLmNsdXN0ZXIubG9jYWwiXSwiZXhwIjoxNjgwODg5NjQ4LCJpYXQiOjE2NDkzNTM2NDgsImlzcyI6Imh0dHBzOi8va3ViZXJuZXRlcy5kZWZhdWx0LnN2Yy5jbHVzdGVyLmxvY2FsIiwia3ViZXJuZXRlcy5pbyI6eyJuYW1lc3BhY2UiOiJ0ZXN0IiwicG9kIjp7Im5hbWUiOiJ2YXVsdC0wIiwidWlkIjoiYTQwNGZiMTktNWQ4MC00OTBlLTkwYjktMGJjNWE3NzA5ODdkIn0sInNlcnZpY2VhY2NvdW50Ijp7Im5hbWUiOiJ2YXVsdCIsInVpZCI6ImI2ZTM2ZDMxLTA2MDQtNDE5MS04Y2JjLTAwYzg4ZWViZDlmOSJ9LCJ3YXJuYWZ0ZXIiOjE2NDkzNTcyNTV9LCJuYmYiOjE2NDkzNTM2NDgsInN1YiI6InN5c3RlbTpzZXJ2aWNlYWNjb3VudDp0ZXN0OnZhdWx0In0.hxzMpKx38rKvaWUBNEg49TioRXt_JT1Z5st4A9NeBWO2xiC8hCDgVJRWqPzejz-sYoQGhZyZcrTa0cbNRIevcR7XH4DnHd27OOzSoj198I2DAdLfw_pntzOjq35-tZhxSYXsfKH69DSpHACpu5HHUAf1aiY3B6cq5Z3gXbtaoHBocfNwvtOirGL8pTYXo1kNCkcahDPfpf3faztyUQ77v0viBKIAqwxDuGks4crqIG5jT_tOnXbb7PahwtE5cS3bMLjQb1j5oEcgq6HF4NMV46Ly479QRoXtYWWsI9OSwl4H7G9Rel3fr9q4IMdCCI5A-FLxL2Fpep9TDwrNQ3mhBQ"
