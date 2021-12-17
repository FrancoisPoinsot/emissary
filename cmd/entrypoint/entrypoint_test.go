package entrypoint

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/datawire/ambassador/v2/pkg/dtest"
	"github.com/datawire/ambassador/v2/pkg/k8s"
	"github.com/datawire/ambassador/v2/pkg/kates"
	"github.com/datawire/ambassador/v2/pkg/kubeapply"
	"github.com/datawire/dlib/dexec"
	"github.com/datawire/dlib/dlog"
	"github.com/stretchr/testify/require"
)

// Test that a rollout of the emissary deployment does not trigger a failure of requests of the clients
func Test_GracefulShutdown(t *testing.T) {
	ctx := dlog.NewTestContext(t, false)
	kubeconfig := dtest.KubeVersionConfig(ctx, dtest.Kube22)
	cli, err := kates.NewClient(kates.ClientConfig{Kubeconfig: kubeconfig})
	require.NoError(t, err)
	setupServerAndIngress(t, ctx, kubeconfig, cli)

	// start a bunch of http requests
	wg, getClientsErrors := launchClients(t, ctx)

	// while the clients are running, trigger a rollout of the ingress
	// we expect that no request will fail
	rolloutRestartIngress(t, ctx, kubeconfig, cli)

	// At this point, the rollout should already be done since rolloutRestartIngress blocks until the deployment is ready
	assertion := func() bool {
		return len(getClientsErrors()) == 0
	}
	assert.Never(t, assertion, 10*time.Second, time.Second)

	// It is probably better to wait for the client to end to avoid report of panic in this test
	wg.Wait()
}

func rolloutRestartIngress(t *testing.T, ctx context.Context, kubeconfig string, cli *kates.Client) {
	dep := &kates.Deployment{
		TypeMeta: kates.TypeMeta{
			Kind: "Deployment",
		},
		ObjectMeta: kates.ObjectMeta{
			Name:      "emissary-ingress-apiext",
			Namespace: "emissary",
		},
	}

	patch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"env": []interface{}{
								map[string]interface{}{
									"name":  "SOMETHING",
									"value": "SOMEVALUE",
								},
							},
							"name": "emissary-ingress-apiext",
						},
					},
				},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, cli.Patch(ctx, dep, kates.StrategicMergePatchType, patch, dep))
}

func launchClients(t *testing.T, ctx context.Context) (wg *sync.WaitGroup, getErrors func() []error) {
	wg = &sync.WaitGroup{}

	errsSync := sync.RWMutex{}
	var errs []error

	appendError := func(err error) {
		defer errsSync.Unlock()
		errsSync.Lock()
		errs = append(errs, err)
	}

	getErrors = func() []error {
		defer errsSync.RUnlock()
		errsSync.RLock()
		return errs

	}

	for i := 0; i <= 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// server is supposed to make the client wait for a few seconds
				// but in case something is wrong, we still want the client to wait a minimum amount of time
				securityTimer := time.After(time.Second)
				err := callServer(t, ctx)
				if err != nil {
					appendError(err)
				}

				select {
				case <-securityTimer:
				case <-ctx.Done():
					break
				}
			}
		}()
	}

	return wg, getErrors
}

func callServer(t *testing.T, ctx context.Context) error {
	req, err := http.NewRequest("GET", "http://localhost:8080", nil)
	require.NoError(t, err)
	req = req.WithContext(ctx)

	// That should make it obvious if any request is interrupted non-gracefully
	req.Header.Set("Requested-Backend-Delay", "5000")
	resp, err := http.DefaultClient.Do(req)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}

	if err != nil {
		return err
	}
	if resp.StatusCode > 300 {
		return errors.Errorf("http client received an invalid status code: %d", resp.StatusCode)
	}

	return nil
}

func setupServerAndIngress(t *testing.T, ctx context.Context, kubeconfig string, cli *kates.Client) {
	require.NoError(t, needsDockerBuilds(ctx, map[string]string{
		"AMBASSADOR_DOCKER_IMAGE": "docker/emissary.docker.push.remote",
		"KAT_SERVER_DOCKER_IMAGE": "docker/kat-server.docker.push.remote",
	}))

	crdFile := "../../manifests/emissary/emissary-crds.yaml"
	aesFile := "../../manifests/emissary/emissary-ingress.yaml"
	aesDat, err := ioutil.ReadFile(aesFile)
	require.NoError(t, err)
	image := os.Getenv("AMBASSADOR_DOCKER_IMAGE")
	require.NotEmpty(t, image)

	aesReplaced := regexp.MustCompile(`docker\.io/emissaryingress/emissary:\S+`).ReplaceAllString(string(aesDat), image)
	newAesFile := filepath.Join(t.TempDir(), "emissary-ingress.yaml")

	require.NoError(t, ioutil.WriteFile(newAesFile, []byte(aesReplaced), 0644))
	kubeinfo := k8s.NewKubeInfo(kubeconfig, "", "")

	namespaceManifest := `
apiVersion: v1
kind: Namespace
metadata:
  name: emissary
`

	require.NoError(t, kubeapply.Kubeapply(ctx, kubeinfo, time.Minute, true, false, crdFile))
	require.NoError(t, kubeapply.Kubeapply(ctx, kubeinfo, time.Minute, true, false, toFile(t, namespaceManifest)))
	require.NoError(t, kubeapply.Kubeapply(ctx, kubeinfo, 2*time.Minute, true, false, newAesFile))

	backendManifest := `
---
apiVersion: v1
kind: Service
metadata:
  name: kat-server
  namespace: emissary
spec:
  ports:
    - name: http
      port: 8080
      targetPort: 8080
  selector:
    app: kat-server
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kat-server
  namespace: emissary
spec:
  replicas: 1
  selector:
    matchLabels:
      app: kat-server
  template:
    metadata:
      labels:
        app: kat-server
    spec:
      containers:
        - name: kat-server
          image: emissary.local/kat-server
          ports:
            - name: http
              containerPort: 8080
          env:
            - name: BACKEND
              value: test
---
apiVersion: getambassador.io/v3alpha1
kind: Mapping
metadata:
  name: kat-server
  namespace: emissary
spec:
  hostname: "*"
  prefix: /
  service: kat-server
  timeout_ms: 30000
`
	require.NoError(t, kubeapply.Kubeapply(ctx, kubeinfo, time.Minute, true, false, toFile(t, backendManifest)))
}

// TODO: factor this with the agent e2e test code
func needsDockerBuilds(ctx context.Context, var2file map[string]string) error {
	var targets []string
	for varname, filename := range var2file {
		if os.Getenv(varname) == "" {
			targets = append(targets, filename)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	if os.Getenv("DEV_REGISTRY") == "" {
		registry := dtest.DockerRegistry(ctx)
		os.Setenv("DEV_REGISTRY", registry)
		os.Setenv("DTEST_REGISTRY", registry)
	}
	cmdline := append([]string{"make", "-C", "../.."}, targets...)
	if err := dexec.CommandContext(ctx, cmdline[0], cmdline[1:]...).Run(); err != nil {
		return err
	}
	for varname, filename := range var2file {
		if os.Getenv(varname) == "" {
			dat, err := ioutil.ReadFile(filepath.Join("../..", filename))
			if err != nil {
				return err
			}
			lines := strings.Split(strings.TrimSpace(string(dat)), "\n")
			if len(lines) < 2 {
				return fmt.Errorf("malformed docker.mk tagfile %q", filename)
			}
			if err := os.Setenv(varname, lines[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func toFile(t *testing.T, text string) string {
	tmpFile, err := ioutil.TempFile(t.TempDir(), "")
	require.NoError(t, err)

	err = ioutil.WriteFile(tmpFile.Name(), []byte(text), 0644)
	require.NoError(t, err)
	return tmpFile.Name()
}
