//go:build integration_tests

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/avast/retry-go/v4"
	"github.com/blang/semver/v4"
	"github.com/kong/kubernetes-testing-framework/pkg/clusters"
	"github.com/kong/kubernetes-testing-framework/pkg/clusters/addons/metallb"
	"github.com/kong/kubernetes-testing-framework/pkg/clusters/types/gke"
	"github.com/kong/kubernetes-testing-framework/pkg/clusters/types/kind"
	"github.com/kong/kubernetes-testing-framework/pkg/environments"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	"github.com/kong/kubernetes-ingress-controller/v3/internal/store"
	testutils "github.com/kong/kubernetes-ingress-controller/v3/internal/util/test"
	"github.com/kong/kubernetes-ingress-controller/v3/test"
	"github.com/kong/kubernetes-ingress-controller/v3/test/consts"
	"github.com/kong/kubernetes-ingress-controller/v3/test/helpers/certificate"
	"github.com/kong/kubernetes-ingress-controller/v3/test/internal/helpers"
	"github.com/kong/kubernetes-ingress-controller/v3/test/internal/testenv"
)

// -----------------------------------------------------------------------------
// Testing Main
// -----------------------------------------------------------------------------

func TestMain(m *testing.M) {
	var code int
	defer func() {
		os.Exit(code)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Logger needs to be configured before anything else happens.
	// This is because the controller manager has a timeout for
	// logger initialization, and if the logger isn't configured
	// after 30s from the start of controller manager package init function,
	// the controller manager will set up a no op logger and continue.
	// The logger cannot be configured after that point.
	logger, logOutput, err := testutils.SetupLoggers("trace", "text")
	if err != nil {
		exitOnErrWithCode(ctx, fmt.Errorf("failed to setup loggers: %w", err), consts.ExitCodeCantCreateLogger)
	}
	if logOutput != "" {
		fmt.Printf("INFO: writing manager logs to %s\n", logOutput)
	}

	fmt.Println("INFO: setting up test environment")
	kongbuilder, extraControllerArgs, err := helpers.GenerateKongBuilder(ctx)
	exitOnErrWithCode(ctx, err, consts.ExitCodeEnvSetupFailed)
	if testenv.KongImage() != "" && testenv.KongTag() != "" {
		fmt.Printf("INFO: custom kong image specified via env: %s:%s\n", testenv.KongImage(), testenv.KongTag())
	}

	// Pin the Helm chart version.
	kongbuilder.WithHelmChartVersion(testenv.KongHelmChartVersion())

	kongAddon := kongbuilder.Build()
	builder := environments.NewBuilder().WithAddons(kongAddon)

	fmt.Println("INFO: configuring cluster for testing environment")
	if existingCluster := testenv.ExistingClusterName(); existingCluster != "" {
		if testenv.ClusterVersion() != "" {
			exitOnErrWithCode(ctx, fmt.Errorf("can't flag cluster version & provide an existing cluster at the same time"), consts.ExitCodeIncompatibleOptions)
		}
		clusterParts := strings.Split(existingCluster, ":")
		if len(clusterParts) != 2 {
			exitOnErrWithCode(ctx, fmt.Errorf("existing cluster in wrong format (%s): format is <TYPE>:<NAME> (e.g. kind:test-cluster)", existingCluster), consts.ExitCodeCantUseExistingCluster)
		}
		clusterType, clusterName := clusterParts[0], clusterParts[1]

		fmt.Printf("INFO: using existing %s cluster %s\n", clusterType, clusterName)
		switch clusterType {
		case string(kind.KindClusterType):
			cluster, err := kind.NewFromExisting(clusterName)
			exitOnErr(ctx, err)
			builder.WithExistingCluster(cluster)
			builder.WithAddons(metallb.New())
		case string(gke.GKEClusterType):
			cluster, err := gke.NewFromExistingWithEnv(ctx, clusterName)
			exitOnErr(ctx, err)
			builder.WithExistingCluster(cluster)
		default:
			exitOnErrWithCode(ctx, fmt.Errorf("unknown cluster type: %s", clusterType), consts.ExitCodeCantUseExistingCluster)
		}
	} else {
		fmt.Println("INFO: no existing cluster found, deploying using Kubernetes In Docker (KIND)")

		builder.WithAddons(metallb.New())

		if testenv.ClusterVersion() != "" {
			clusterVersion, err := semver.Parse(strings.TrimPrefix(testenv.ClusterVersion(), "v"))
			exitOnErr(ctx, err)

			fmt.Printf("INFO: build a new KIND cluster with version %s\n", clusterVersion.String())
			builder.WithKubernetesVersion(clusterVersion)
		}
	}

	fmt.Println("INFO: building test environment")
	env, err = builder.Build(ctx)
	exitOnErr(ctx, err)

	cleaner := clusters.NewCleaner(env.Cluster())
	defer func() {
		if err := cleaner.Cleanup(ctx); err != nil {
			fmt.Printf("ERROR: failed cleaning up the cluster: %v\n", err)
		}
	}()

	exitOnErr(ctx, DeployAddonsForCluster(ctx, env.Cluster()))
	fmt.Printf("INFO: waiting for cluster %s and all addons to become ready\n", env.Cluster().Name())
	envReadyCtx, envReadyCancel := context.WithTimeout(ctx, testenv.EnvironmentReadyTimeout())
	defer envReadyCancel()
	exitOnErr(ctx, <-env.WaitForReady(envReadyCtx))

	fmt.Println("INFO: collecting urls from the kong proxy deployment")
	proxyURL, err = kongAddon.ProxyURL(ctx, env.Cluster())
	exitOnErr(ctx, err)
	proxyAdminURL, err = kongAddon.ProxyAdminURL(ctx, env.Cluster())
	exitOnErr(ctx, err)
	proxyUDPURL, err = kongAddon.ProxyUDPURL(ctx, env.Cluster())
	exitOnErr(ctx, err)

	exitOnErr(
		ctx,
		retry.Do(
			func() error {
				reqCtx, cancel := context.WithTimeout(ctx, test.RequestTimeout)
				defer cancel()
				kongVersion, err := helpers.ValidateMinimalSupportedKongVersion(reqCtx, proxyAdminURL, consts.KongTestPassword)
				if err != nil {
					return err
				}
				fmt.Printf("INFO: using Kong instance (version: %q) reachable at %s\n", kongVersion, proxyAdminURL)
				return nil
			},
			retry.OnRetry(
				func(n uint, err error) {
					fmt.Printf("WARNING: try to get Kong Gateway version attempt %d/10 - error: %s\n", n+1, err)
				},
			),
			retry.LastErrorOnly(true), retry.RetryIf(func(err error) bool {
				return !errors.As(err, &helpers.TooOldKongGatewayError{})
			}),
		))

	if v := os.Getenv("KONG_BRING_MY_OWN_KIC"); v == "true" {
		fmt.Println("WARNING: caller indicated that they will manage their own controller")
	} else {
		fmt.Println("INFO: creating additional controller namespaces")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: consts.ControllerNamespace}}
		if _, err := env.Cluster().Client().CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				exitOnErr(ctx, err)
			}
		}
		fmt.Println("INFO: configuring feature gates")
		featureGates := testenv.ControllerFeatureGates()
		fmt.Printf("INFO: feature gates enabled: %s\n", featureGates)
		fmt.Println("INFO: starting the controller manager")
		cert, key := certificate.GetKongSystemSelfSignedCerts()
		standardControllerArgs := []string{
			fmt.Sprintf("--ingress-class=%s", consts.IngressClass),
			fmt.Sprintf("--admission-webhook-cert=%s", cert),
			fmt.Sprintf("--admission-webhook-key=%s", key),
			fmt.Sprintf("--admission-webhook-listen=0.0.0.0:%d", testutils.AdmissionWebhookListenPort),
			"--profiling",
			"--dump-config",
			"--log-level=trace", // not used, as controller logger is configured separately
			"--anonymous-reports=false",
			fmt.Sprintf("--feature-gates=%s", featureGates),
			fmt.Sprintf("--election-namespace=%s", kongAddon.Namespace()),
		}
		allControllerArgs := append(standardControllerArgs, extraControllerArgs...)
		exitOnErr(ctx, testutils.DeployControllerManagerForCluster(ctx, logger, env.Cluster(), allControllerArgs...))
	}

	gatewayClient, err := gatewayclient.NewForConfig(env.Cluster().Config())
	exitOnErr(ctx, err)

	fmt.Println("INFO: Deploying the default GatewayClass")
	gwc, err := DeployGatewayClass(ctx, gatewayClient, unmanagedGatewayClassName)
	exitOnErr(ctx, err)
	cleaner.Add(gwc)

	fmt.Printf("INFO: Deploying the controller's IngressClass %q\n", consts.IngressClass)
	createIngressClass := func() *netv1.IngressClass {
		return &netv1.IngressClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: consts.IngressClass,
			},
			Spec: netv1.IngressClassSpec{
				Controller: store.IngressClassKongController,
			},
		}
	}
	ingClasses := env.Cluster().Client().NetworkingV1().IngressClasses()
	_, err = ingClasses.Create(ctx, createIngressClass(), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// If for some reason the ingress class is already in the cluster don't
		// fail the whole test suite but recreate it and continue.
		err = ingClasses.Delete(ctx, consts.IngressClass, metav1.DeleteOptions{})
		exitOnErr(ctx, err)
		_, err = ingClasses.Create(ctx, createIngressClass(), metav1.CreateOptions{})
		exitOnErr(ctx, err)
	}
	exitOnErr(ctx, err)
	defer func() {
		// deleting this directly instead of adding it to the cleaner because
		// the cleaner always gets a 404 on it for unknown reasons
		_ = ingClasses.Delete(ctx, consts.IngressClass, metav1.DeleteOptions{})
	}()

	if os.Getenv("TEST_RUN_INVALID_CONFIG_CASES") == "true" {
		fmt.Println("INFO: run tests with invalid configurations")
		fmt.Println("WARN: should run these cases separately to prevent config being affected by invalid cases")
		runInvalidConfigTests = true
	}

	clusterVersion, err := env.Cluster().Version()
	exitOnErr(ctx, err)

	fmt.Printf("INFO: testing environment is ready KUBERNETES_VERSION=(%v): running tests\n", clusterVersion)
	code = m.Run()

	if testenv.IsCI() {
		fmt.Printf("INFO: running in ephemeral CI environment, skipping cluster %s teardown\n", env.Cluster().Name())
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), test.EnvironmentCleanupTimeout)
		defer cancel()
		exitOnErr(ctx, helpers.RemoveCluster(ctx, env.Cluster()))
	}
}
