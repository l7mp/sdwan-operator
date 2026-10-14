package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	opv1a1 "github.com/l7mp/dcontroller/pkg/api/operator/v1alpha1"
	"github.com/l7mp/dcontroller/pkg/apiserver"
	"github.com/l7mp/dcontroller/pkg/cache"
	"github.com/l7mp/dcontroller/pkg/object"
	doperator "github.com/l7mp/dcontroller/pkg/operator"
	dreconciler "github.com/l7mp/dcontroller/pkg/reconciler"

	"github.com/l7mp/sdwan-operator/internal/sdwan"
)

const (
	SDWANOperatorSpec               = "artifacts/endpoints-controller-spec.yaml"
	SDWANOperatorGatherSpec         = "artifacts/endpoints-controller-gather-spec.yaml"
	SDWANPolicyTunnelAnnotationName = "policy.sdwan.cisco.com/tunnel"
	SDWANConfigFile                 = "vmanage-config.yaml"
	apiServerPort                   = 8443
	maxPort                         = 65535
	retryBaseDelay                  = time.Second
	retryMaxDelay                   = 5 * time.Minute
)

var (
	scheme                         = runtime.NewScheme()
	disableEndpointPooling, dryRun *bool
	configFile                     *string
	server                         *apiserver.APIServer
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func NewOperator(specFile string, logger logr.Logger) (*doperator.Operator, error) {
	log := logger.WithName("sdwan-op")
	ctrl.SetLogger(log)

	// Create a dmanager
	config := ctrl.GetConfigOrDie()
	api, err := cache.NewAPI(config, cache.APIOptions{
		CacheOptions: cache.CacheOptions{Logger: logger},
	})
	if err != nil {
		return nil, err
	}

	apiServerConfig, err := apiserver.NewDefaultConfig("", apiServerPort, api.Client, true, false, logger)
	if err != nil {
		return nil, err
	}
	server, err = apiserver.NewAPIServer(apiServerConfig)
	if err != nil {
		return nil, err
	}

	// Load the operator from file
	errorChan := make(chan error, 16)
	opts := doperator.Options{
		Cache:        api.GetCache(),
		ErrorChannel: errorChan,
		APIServer:    server,
		Logger:       logger,
	}

	op, err := doperator.NewFromFile("sdwan-operator", config, specFile, opts)
	if err != nil {
		return nil, fmt.Errorf("unable to create SDWAN operator: %w", err)
	}

	// Read vManage config
	vManageConf := &sdwan.Config{DryRun: *dryRun}
	if !*dryRun {
		c, err := sdwan.ReadConfig(*configFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read vManage config: %w", err)
		}
		vManageConf = c
	}

	// Create the SD-WAN policy controller
	if _, err := NewPolicyController(op.GetManager(), logger, *vManageConf); err != nil {
		return nil, fmt.Errorf("failed to create policy controller: %w", err)
	}

	log.Info("created SDWAN policy controller")

	return op, nil
}

// policyController implements the policy controller
type policyController struct {
	client.Client
	log          logr.Logger
	sdwanManager sdwan.Manager
}

func NewPolicyController(mgr manager.Manager, log logr.Logger, sdwanConf sdwan.Config) (*policyController, error) {
	m, err := sdwan.NewManager(sdwanConf, log.WithName("sdwan-mngr"))
	if err != nil {
		return nil, err
	}

	r := &policyController{
		Client:       mgr.GetClient(),
		log:          log.WithName("policy-ctrl"),
		sdwanManager: m,
	}

	on := true
	c, err := controller.NewTyped("sdwan-policy-controller", mgr, controller.TypedOptions[dreconciler.Request]{
		SkipNameValidation: &on,
		Reconciler:         r,
		// Every attempt deactivates and activates the central policy, so
		// retry far less eagerly than the 5ms default.
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[dreconciler.Request](retryBaseDelay, retryMaxDelay),
	})
	if err != nil {
		return nil, err
	}

	src, err := dreconciler.NewSource(mgr, "sdwan-operator", opv1a1.Source{
		Resource: opv1a1.Resource{
			Kind: "TunnelPolicyView",
		},
	}).GetSource()
	if err != nil {
		return nil, fmt.Errorf("failed to create source: %w", err)
	}

	if err := c.Watch(src); err != nil {
		return nil, fmt.Errorf("failed to create watch: %w", err)
	}
	r.log.Info("created SDWAN policy controller")

	return r, nil
}

func (r *policyController) Reconcile(ctx context.Context, req dreconciler.Request) (reconcile.Result, error) {
	r.log.Info("Reconciling", "request", req.String())

	if err := r.reconcileEvent(ctx, req); err != nil {
		// A policy that cannot be applied as specified is not retried; any
		// other error is, with backoff.
		if errors.Is(err, sdwan.ErrInvalidSpec) {
			err = reconcile.TerminalError(err)
		}
		return reconcile.Result{}, err
	}

	r.log.Info("Reconciliation done")

	return reconcile.Result{}, nil
}

func (r *policyController) reconcileEvent(ctx context.Context, req dreconciler.Request) error {
	// vManage update
	switch req.EventType {
	case object.Added, object.Updated, object.Upserted:
		obj := object.NewViewObject("sdwan-operator", req.GVK.Kind)
		if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				// deleted since: its delete event removes the SD-WAN resources
				r.log.Info("SD-WAN tunnel policy gone, skipping", "name", req.Name, "namespace", req.Namespace)
				return nil
			}
			return fmt.Errorf("failed to get added/updated object: %w", err)
		}

		spec, ok, err := unstructured.NestedMap(obj.Object, "spec")
		if err != nil || !ok {
			return fmt.Errorf("%w: failed to look up added/updated object spec: %q", sdwan.ErrInvalidSpec, object.Dump(obj))
		}

		name := obj.GetName()
		namespace := obj.GetNamespace()

		r.log.Info("Add/update SD-WAN tunnel policy", "name", name, "namespace", namespace,
			"spec", fmt.Sprintf("%#v", spec))

		// Must use endoint-pooling when using a real manager
		if *dryRun || *disableEndpointPooling {
			return nil
		}

		port, protocol, tunnel, endpoints, err := parseSpec(spec)
		if err != nil {
			return fmt.Errorf("%w: %w", sdwan.ErrInvalidSpec, err)
		}

		if err := r.sdwanManager.HandleUpsertEvent(namespace, name, endpoints, port, protocol, tunnel); err != nil {
			return fmt.Errorf("failed to upsert SD-WAN resources: %w", err)
		}

	case object.Deleted:
		r.log.Info("Delete SD-WAN tunnel policy", "name", req.Name, "namespace", req.Namespace)

		// Must use endoint-pooling when using a real manager
		if *dryRun || *disableEndpointPooling {
			return nil
		}

		if err := r.sdwanManager.HandleDeleteEvent(req.Namespace, req.Name); err != nil {
			return fmt.Errorf("failed to delete SD-WAN resources: %w", err)
		}

	default:
		r.log.Info("Unhandled event for SD-WAN tunnel policy", "name", req.Name, "namespace", req.Namespace, "type", req.EventType)
	}

	return nil
}

// parseSpec returns the fields of a tunnel policy spec.
func parseSpec(spec map[string]interface{}) (port int64, protocol, tunnel string, endpoints []string, err error) {
	port, err = getTargetPort(spec)
	if err != nil {
		return 0, "", "", nil, fmt.Errorf("unable to parse targetPort from spec: %w", err)
	}

	protocol, ok, err := unstructured.NestedString(spec, "protocol")
	if err != nil || !ok {
		return 0, "", "", nil, fmt.Errorf("unable to parse protocol from spec: %v", err)
	}

	tunnel, ok, err = unstructured.NestedString(spec, "tunnel")
	if err != nil || !ok {
		return 0, "", "", nil, fmt.Errorf("unable to parse tunnel from spec: %v", err)
	}

	addresses, ok, err := unstructured.NestedSlice(spec, "addresses")
	if err != nil || !ok {
		return 0, "", "", nil, fmt.Errorf("unable to parse addresses from spec: %v", err)
	}

	endpoints = make([]string, 0, len(addresses))
	for i, val := range addresses {
		address, ok := val.(string)
		if !ok {
			return 0, "", "", nil, fmt.Errorf("unable to parse address %d from spec: expected string, got %T", i, val)
		}
		endpoints = append(endpoints, address)
	}

	return port, protocol, tunnel, endpoints, nil
}

// getTargetPort returns the target port of a spec. A Service gives it as a
// number or a string, and a number may be decoded as an integer or a float.
// Named ports are not supported.
func getTargetPort(spec map[string]interface{}) (int64, error) {
	raw, ok := spec["targetPort"]
	if !ok {
		return 0, fmt.Errorf("field missing")
	}

	var port int64
	switch v := raw.(type) {
	case int64:
		port = v
	case int:
		port = int64(v)
	case int32:
		port = int64(v)
	case float64:
		if v != math.Trunc(v) || v < 1 || v > maxPort {
			return 0, fmt.Errorf("invalid port %v", v)
		}
		port = int64(v)
	case string:
		p, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("named port %q is not supported, use a port number", v)
		}
		port = p
	default:
		return 0, fmt.Errorf("unexpected type %T", raw)
	}
	if port < 1 || port > maxPort {
		return 0, fmt.Errorf("invalid port %d", port)
	}

	return port, nil
}

func main() {
	disableEndpointPooling = flag.Bool("disable-endpoint-pooling", false,
		"Generate per-endpoint objects instead of a single object listing all service endpoints.")
	dryRun = flag.Bool("dry-run", false, "Suppress SD-WAN policy updates.")

	configFile = flag.String("config-file", SDWANConfigFile, "Config file path")

	zapOpts := zap.Options{
		Development:     true,
		DestWriter:      os.Stderr,
		StacktraceLevel: zapcore.Level(3),
		TimeEncoder:     zapcore.RFC3339NanoTimeEncoder,
	}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&zapOpts))
	log := logger.WithName("setup")
	slogHandler := logr.ToSlogHandler(log)
	slog.SetDefault(slog.New(slogHandler))

	if *dryRun {
		log.Info("dry-run mode enabled")
	}

	// Define the controller pipeline
	specFile := SDWANOperatorGatherSpec
	if *disableEndpointPooling {
		specFile = SDWANOperatorSpec
	}

	ctx := ctrl.SetupSignalHandler()

	op, err := NewOperator(specFile, logger)
	if err != nil {
		log.Error(err, "problem running operator")
		os.Exit(1)
	}

	go func() {
		if err := server.Start(ctx); err != nil {
			log.Error(err, "problem running operator")
		}
	}()

	// Create an error reporter thread
	go func() {
		for {
			select {
			case <-ctx.Done():
				os.Exit(1)
			case err := <-op.GetErrorChannel():
				log.Error(err, "operator error")
			}
		}
	}()

	if err := op.Start(ctx); err != nil {
		log.Error(err, "problem running operator")
		os.Exit(1)
	}
}
