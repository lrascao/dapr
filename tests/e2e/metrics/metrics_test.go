//go:build e2e
// +build e2e

/*
Copyright 2021 The Dapr Authors
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

package metrics_e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	commonv1pb "github.com/dapr/dapr/pkg/proto/common/v1"
	pb "github.com/dapr/dapr/pkg/proto/runtime/v1"
	"github.com/dapr/dapr/tests/e2e/utils"
	kube "github.com/dapr/dapr/tests/platforms/kubernetes"
	"github.com/dapr/dapr/tests/runner"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type testCommandRequest struct {
	Message string `json:"message,omitempty"`
}

const numHealthChecks = 60 // Number of times to check for endpoint health per app.

var tr *runner.TestRunner

func TestMain(m *testing.M) {
	utils.SetupLogs("metrics")
	utils.InitHTTPClient(false)

	// This test shows how to deploy the multiple test apps, validate the side-car injection
	// and validate the response by using test app's service endpoint

	// These apps will be deployed for hellodapr test before starting actual test
	// and will be cleaned up after all tests are finished automatically
	testApps := []kube.AppDescription{
		{
			AppName:        "httpmetrics",
			DaprEnabled:    true,
			ImageName:      "e2e-hellodapr",
			Config:         "metrics-config",
			Replicas:       1,
			IngressEnabled: true,
			MetricsEnabled: true,
		},
		{
			AppName:        "grpcmetrics",
			DaprEnabled:    true,
			ImageName:      "e2e-stateapp",
			Config:         "metrics-config",
			Replicas:       1,
			IngressEnabled: true,
			MetricsEnabled: true,
		},
		{
			AppName:        "disabledmetric",
			Config:         "disable-telemetry",
			DaprEnabled:    true,
			ImageName:      "e2e-hellodapr",
			Replicas:       1,
			IngressEnabled: true,
			MetricsEnabled: true,
		},
	}

	tr = runner.NewTestRunner("metrics", testApps, nil, nil)
	os.Exit(tr.Start(m))
}

type testCase struct {
	name          string
	app           string
	protocol      string
	action        func(t *testing.T, app string, n, port int)
	actionInvokes int
	evaluate      func(t *testing.T, app string, res *http.Response)
}

var metricsTests = []testCase{
	{
		"http metrics",
		"httpmetrics",
		"http",
		invokeDaprHTTP,
		3,
		testHTTPMetrics,
	},
	{
		"grpc metrics",
		"grpcmetrics",
		"grpc",
		invokeDaprGRPC,
		10,
		testGRPCMetrics,
	},
	{
		"metric off",
		"disabledmetric",
		"http",
		invokeDaprHTTP,
		3,
		testMetricDisabled,
	},
}

func TestMetrics(t *testing.T) {
	for _, tt := range metricsTests {
		// Open connection to the app on the dapr port and metrics port.
		// These will only be closed when the test runner is disposed after
		// all tests are run.
		var targetDaprPort int
		if tt.protocol == "http" {
			targetDaprPort = 3500
		} else if tt.protocol == "grpc" {
			targetDaprPort = 50001
		}
		localPorts, err := tr.Platform.PortForwardToApp(tt.app, targetDaprPort, 9090)
		require.NoError(t, err)

		// Port order is maintained when opening connection
		daprPort := localPorts[0]
		metricsPort := localPorts[1]

		t.Run(tt.name, func(t *testing.T) {
			// Perform an action n times using the Dapr API
			tt.action(t, tt.app, tt.actionInvokes, daprPort)

			// Get the metrics from the metrics endpoint
			res, err := utils.HTTPGetRawNTimes(fmt.Sprintf("http://localhost:%v", metricsPort), numHealthChecks)
			require.NoError(t, err)
			defer func() {
				// Drain before closing
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
			}()

			// Evaluate the metrics are as expected
			tt.evaluate(t, tt.app, res)
		})
	}
}

func invokeDaprHTTP(t *testing.T, app string, n, daprPort int) {
	body, err := json.Marshal(testCommandRequest{
		Message: "Hello Dapr.",
	})
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		// We don't evaluate the response here as we're only testing the metrics
		_, err = utils.HTTPPost(fmt.Sprintf("http://localhost:%d/v1.0/invoke/%s/method/tests/green", daprPort, app), body)
		require.NoError(t, err)
	}
}

func testHTTPMetrics(t *testing.T, app string, res *http.Response) {
	require.NotNil(t, res)

	foundMetric := findHTTPMetricFromPrometheus(t, app, res)

	// Check metric was found
	require.True(t, foundMetric)
}

func testMetricDisabled(t *testing.T, app string, res *http.Response) {
	require.NotNil(t, res)

	foundMetric := findHTTPMetricFromPrometheus(t, app, res)

	// Check metric was found
	require.False(t, foundMetric)
}

func findHTTPMetricFromPrometheus(t *testing.T, app string, res *http.Response) (foundMetric bool) {
	rfmt := expfmt.ResponseFormat(res.Header)
	require.NotEqual(t, rfmt.FormatType(), expfmt.TypeUnknown)

	decoder := expfmt.NewDecoder(res.Body, rfmt)

	// This test will loop through each of the metrics and look for a specifc
	// metric `dapr_http_server_request_count`.
	var foundGet, foundPost bool
	for {
		mf := &io_prometheus_client.MetricFamily{}
		err := decoder.Decode(mf)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		if strings.ToLower(mf.GetName()) == "dapr_http_server_request_count" {
			foundMetric = true

			for _, m := range mf.GetMetric() {
				if m == nil {
					continue
				}
				count := m.GetCounter()

				// check metrics with expected method exists
				for _, l := range m.GetLabel() {
					if l == nil {
						continue
					}
					val := l.GetValue()
					switch strings.ToLower(l.GetName()) {
					case "app_id":
						assert.Equal(t, "httpmetrics", val)
					case "method":
						if count.GetValue() > 0 {
							switch val {
							case "GET":
								foundGet = true
							case "POST":
								foundPost = true
							}
						}
					}
				}
			}
		}
	}

	if foundMetric {
		require.True(t, foundGet)
		require.True(t, foundPost)
	}

	return foundMetric
}

func invokeDaprGRPC(t *testing.T, app string, n, daprPort int) {
	daprAddress := fmt.Sprintf("localhost:%d", daprPort)
	conn, err := grpc.Dial(daprAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	client := pb.NewDaprClient(conn)

	for i := 0; i < n; i++ {
		_, err = client.SaveState(t.Context(), &pb.SaveStateRequest{
			StoreName: "statestore",
			States: []*commonv1pb.StateItem{
				{
					Key:   "myKey",
					Value: []byte("My State"),
				},
			},
		})
		require.NoError(t, err)
	}
}

func testGRPCMetrics(t *testing.T, app string, res *http.Response) {
	require.NotNil(t, res)

	rfmt := expfmt.ResponseFormat(res.Header)
	require.NotEqual(t, rfmt.FormatType(), expfmt.TypeUnknown)

	decoder := expfmt.NewDecoder(res.Body, rfmt)

	// This test will loop through each of the metrics and look for a specifc
	// metric `dapr_grpc_io_server_completed_rpcs`. This metric will exist for
	// multiple `grpc_server_method` labels, therefore, we loop through the labels
	// to find the instance that has `grpc_server_method="SaveState". Once we
	// find the desired metric entry, we check the metric's value is as expected.`
	var foundMetric bool
	var foundMethod bool
	for {
		mf := &io_prometheus_client.MetricFamily{}
		err := decoder.Decode(mf)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)

		if strings.EqualFold(mf.GetName(), "dapr_grpc_io_server_completed_rpcs") {
			foundMetric = true
			for _, m := range mf.GetMetric() {
				if m == nil {
					continue
				}
				// Check path label is as expected
				for _, l := range m.GetLabel() {
					if l == nil {
						continue
					}

					if strings.EqualFold(l.GetName(), "grpc_server_method") {
						if strings.EqualFold(l.GetValue(), "/dapr.proto.runtime.v1.Dapr/SaveState") {
							foundMethod = true

							// Check value is as expected
							require.Equal(t, 10, int(m.GetCounter().GetValue()))
							break
						}
					}
				}
			}
		}
	}
	// Check metric was found
	require.True(t, foundMetric)
	// Check path label was found
	require.True(t, foundMethod)
}
