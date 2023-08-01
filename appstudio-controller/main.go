/*
Copyright 2022.

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

package main

import (
	"flag"
	"os"
	"strings"

	codereadytoolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	applicationv1alpha1 "github.com/redhat-appstudio/application-api/api/v1alpha1"
	gitopsdeploymentv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"

	appstudioredhatcomcontrollers "github.com/redhat-appstudio/managed-gitops/appstudio-controller/controllers/appstudio.redhat.com"

	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	logutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util/log"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.
			WithName(logutil.LogLogger_managed_gitops)
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(applicationv1alpha1.AddToScheme(scheme))
	// utilruntime.Must(managedgitopsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gitopsdeploymentv1alpha1.AddToScheme(scheme))
	utilruntime.Must(applicationv1alpha1.AddToScheme(scheme))
	utilruntime.Must(codereadytoolchainv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var profilerAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8084", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8085", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&profilerAddr, "profiler-address", ":6062", "The address for serving pprof profiles")

	opts := crzap.Options{
		TimeEncoder: zapcore.ISO8601TimeEncoder,
		ZapOpts: []zap.Option{
			zap.WithCaller(true),
		},
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(crzap.New(crzap.UseFlagOptions(&opts)))

	if sharedutil.IsProfilingEnabled() {
		setupLog.Info("Starting pprof profiler server", "address", profilerAddr)
		go sharedutil.StartProfilers(profilerAddr)
	}

	ctx := ctrl.SetupSignalHandler()

	restConfig, err := sharedutil.GetRESTConfig()
	if err != nil {
		setupLog.Error(err, "unable to get kubeconfig")
		os.Exit(1)
		return
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "53746cb8.redhat.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&appstudioredhatcomcontrollers.ApplicationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Application")
		os.Exit(1)
	}
	if err = (&appstudioredhatcomcontrollers.SnapshotReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Snapshot")
		os.Exit(1)
	}
	if err = (&appstudioredhatcomcontrollers.PromotionRunReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PromotionRun")
		os.Exit(1)
	}
	if err = (&appstudioredhatcomcontrollers.SnapshotEnvironmentBindingReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SnapshotEnvironmentBinding")
		os.Exit(1)
	}
	if err = (&appstudioredhatcomcontrollers.EnvironmentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Environment")
		os.Exit(1)
	}

	// If the webhook is not disabled, start listening on the webhook URL
	if !strings.EqualFold(os.Getenv("DISABLE_APPSTUDIO_WEBHOOK"), "true") {

		setupLog.Info("setting up webhooks")
		if err = (&applicationv1alpha1.Snapshot{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "Snapshot")
			os.Exit(1)
		}

		if err = (&applicationv1alpha1.SnapshotEnvironmentBinding{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "SnapshotEnvironmentBinding")
			os.Exit(1)
		}

		if err = (&applicationv1alpha1.PromotionRun{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "PromotionRun")
			os.Exit(1)
		}

		// if err = (&applicationv1alpha1.Environment{}).SetupWebhookWithManager(mgr); err != nil {
		// 	setupLog.Error(err, "unable to create webhook", "webhook", "Environment")
		// 	os.Exit(1)
		// }
	}

	if err = (&appstudioredhatcomcontrollers.DeploymentTargetClaimReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DeploymentTargetClaim")
		os.Exit(1)
	}

	if err = (&appstudioredhatcomcontrollers.DeploymentTargetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Clock:  sharedutil.NewClock(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DeploymentTarget")
		os.Exit(1)
	}

	if err = (&appstudioredhatcomcontrollers.SandboxProvisionerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SandboxProvisioner")
		os.Exit(1)
	}

	if err = (&appstudioredhatcomcontrollers.DevsandboxDeploymentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DevsandboxDeployment")
		os.Exit(1)
	}

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
