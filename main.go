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
	"sync"
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
	tunnelPolicyKind                = "TunnelPolicyView"
	retryBaseDelay                  = time.Second
	retryMaxDelay                   = 5 * time.Minute
	// maxAttempts is the number of attempts of a change before giving up.
	maxAttempts = 10
	// batchDelay is how long a batch waits for more changes after the first
	// one, so that a burst of changes makes one batch.
	batchDelay = time.Second
	// maxBatch bounds the changes in a batch, so that its cycle, mostly the
	// two task waits of at most 5 minutes, fits in changeTimeout: each change
	// takes one or two vManage calls.
	maxBatch = 50
	// changeTimeout bounds the wait for a change at shutdown: a change waits
	// for the vManage tasks twice, at most 5 minutes each.
	changeTimeout = 11 * time.Minute
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

func NewOperator(specFile string, logger logr.Logger) (*doperator.Operator, *policyController, error) {
	log := logger.WithName("sdwan-op")
	ctrl.SetLogger(log)

	// Create a dmanager
	config := ctrl.GetConfigOrDie()
	api, err := cache.NewAPI(config, cache.APIOptions{
		CacheOptions: cache.CacheOptions{Logger: logger},
	})
	if err != nil {
		return nil, nil, err
	}

	apiServerConfig, err := apiserver.NewDefaultConfig("", apiServerPort, api.Client, true, false, logger)
	if err != nil {
		return nil, nil, err
	}
	server, err = apiserver.NewAPIServer(apiServerConfig)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("unable to create SDWAN operator: %w", err)
	}

	// Read vManage config
	vManageConf := &sdwan.Config{DryRun: *dryRun}
	if !*dryRun {
		c, err := sdwan.ReadConfig(*configFile)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to read vManage config: %w", err)
		}
		vManageConf = c
	}

	// Create the SD-WAN policy controller
	pc, err := NewPolicyController(op.GetManager(), logger, *vManageConf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create policy controller: %w", err)
	}

	log.Info("created SDWAN policy controller")

	return op, pc, nil
}

// policyController applies the tunnel policies to vManage in batches: one
// cycle of the central policy for all the changes queued since the last one.
type policyController struct {
	client.Client
	log          logr.Logger
	sdwanManager sdwan.Manager
	// queue holds the tunnel policies to apply. An object queued while it is
	// being applied is applied again after, once.
	queue      workqueue.TypedRateLimitingInterface[types.NamespacedName]
	batchDelay time.Duration
	// changing is read-locked while an SD-WAN change is in progress.
	changing sync.RWMutex
}

func newChangeQueue(baseDelay, maxDelay time.Duration) workqueue.TypedRateLimitingInterface[types.NamespacedName] {
	return workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemExponentialFailureRateLimiter[types.NamespacedName](baseDelay, maxDelay),
		workqueue.TypedRateLimitingQueueConfig[types.NamespacedName]{Name: "sdwan-changes"})
}

// waitChanges waits until no SD-WAN change is in progress, for at most
// timeout, and keeps new changes from starting. It returns false on timeout.
func (r *policyController) waitChanges(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		r.changing.Lock()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
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
		// Every attempt deactivates and activates the central policy, so
		// retry far less eagerly than the 5ms default.
		queue:      newChangeQueue(retryBaseDelay, retryMaxDelay),
		batchDelay: batchDelay,
	}
	if err := mgr.Add(manager.RunnableFunc(r.runBatches)); err != nil {
		return nil, err
	}

	on := true
	c, err := controller.NewTyped("sdwan-policy-controller", mgr, controller.TypedOptions[dreconciler.Request]{
		SkipNameValidation: &on,
		Reconciler:         r,
	})
	if err != nil {
		return nil, err
	}

	src, err := dreconciler.NewSource(mgr, "sdwan-operator", opv1a1.Source{
		Resource: opv1a1.Resource{
			Kind: tunnelPolicyKind,
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

// Reconcile queues a tunnel policy for the next batch. The batch reads its
// state then, so a policy changed many times in the meantime is applied once.
func (r *policyController) Reconcile(_ context.Context, req dreconciler.Request) (reconcile.Result, error) {
	r.log.V(1).Info("Queueing", "request", req.String())
	r.queue.Add(types.NamespacedName{Namespace: req.Namespace, Name: req.Name})
	return reconcile.Result{}, nil
}

// runBatches applies the queued tunnel policies in batches until ctx is done.
func (r *policyController) runBatches(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		r.queue.ShutDown()
	}()
	for r.applyBatch(ctx) {
	}
	return nil
}

// applyBatch applies the next batch of queued tunnel policies. It returns
// false once the controller is stopping.
func (r *policyController) applyBatch(ctx context.Context) bool {
	batch := r.nextBatch(ctx)
	defer func() {
		for _, key := range batch {
			r.queue.Done(key)
		}
	}()
	if len(batch) == 0 || ctx.Err() != nil {
		return false
	}

	// A change interrupted between deactivating and activating the central
	// policy would leave it off: shutdown waits for it.
	r.changing.RLock()
	defer r.changing.RUnlock()

	var keys []types.NamespacedName
	var changes []sdwan.Change
	for _, key := range batch {
		change, err := r.currentChange(ctx, key)
		if err != nil {
			r.finish(key, err)
			continue
		}
		keys = append(keys, key)
		changes = append(changes, change)
	}
	if len(changes) == 0 {
		return true
	}

	var upserts, deletes []string
	for _, c := range changes {
		if c.Delete {
			deletes = append(deletes, c.Namespace+"/"+c.Name)
		} else {
			upserts = append(upserts, c.Namespace+"/"+c.Name)
		}
	}
	r.log.Info("Applying SD-WAN changes", "upserts", upserts, "deletes", deletes)

	errs := r.sdwanManager.Apply(changes)
	failed := 0
	for i, key := range keys {
		if errs[i] != nil {
			failed++
		}
		r.finish(key, errs[i])
	}
	r.log.Info("SD-WAN changes applied", "done", len(keys)-failed, "failed", failed)

	return true
}

// nextBatch waits for a queued tunnel policy, then for more during the
// batch delay, and returns at most maxBatch of them. It returns nothing once
// the queue is shut down.
func (r *policyController) nextBatch(ctx context.Context) []types.NamespacedName {
	key, shutdown := r.queue.Get()
	if shutdown {
		return nil
	}
	batch := []types.NamespacedName{key}

	select {
	case <-ctx.Done():
	case <-time.After(r.batchDelay):
	}
	for len(batch) < maxBatch && r.queue.Len() > 0 {
		key, shutdown := r.queue.Get()
		if shutdown {
			break
		}
		batch = append(batch, key)
	}
	return batch
}

// currentChange returns the change that applies the current state of a
// tunnel policy: a delete if it is gone.
func (r *policyController) currentChange(ctx context.Context, key types.NamespacedName) (sdwan.Change, error) {
	obj := object.NewViewObject("sdwan-operator", tunnelPolicyKind)
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return sdwan.Change{Namespace: key.Namespace, Name: key.Name, Delete: true}, nil
		}
		return sdwan.Change{}, fmt.Errorf("failed to get tunnel policy: %w", err)
	}

	spec, ok, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !ok {
		return sdwan.Change{}, fmt.Errorf("%w: failed to look up object spec: %q", sdwan.ErrInvalidSpec, object.Dump(obj))
	}
	r.log.Info("SD-WAN tunnel policy", "name", key.Name, "namespace", key.Namespace,
		"spec", fmt.Sprintf("%#v", spec))

	port, protocol, tunnel, endpoints, err := parseSpec(spec)
	if err != nil {
		return sdwan.Change{}, fmt.Errorf("%w: %w", sdwan.ErrInvalidSpec, err)
	}

	return sdwan.Change{Namespace: key.Namespace, Name: key.Name, Endpoints: endpoints,
		TargetPort: port, Protocol: protocol, Tunnel: tunnel}, nil
}

// finish ends an attempt to apply a tunnel policy. A policy that failed is
// retried with backoff, unless it cannot be applied as specified, or failed
// maxAttempts times in a row: every attempt toggles the central policy.
func (r *policyController) finish(key types.NamespacedName, err error) {
	attempt := r.queue.NumRequeues(key) + 1
	switch {
	case err == nil:
		r.queue.Forget(key)
	case errors.Is(err, sdwan.ErrInvalidSpec):
		r.queue.Forget(key)
		r.log.Error(err, "invalid SD-WAN tunnel policy, not retrying", "name", key.String())
	case attempt >= maxAttempts:
		r.queue.Forget(key)
		r.log.Error(err, fmt.Sprintf("giving up on SD-WAN tunnel policy after %d attempts", attempt), "name", key.String())
	default:
		r.log.Error(err, "SD-WAN tunnel policy failed, retrying", "name", key.String(), "attempt", attempt)
		r.queue.AddRateLimited(key)
	}
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

	// without endpoint pooling, a spec has a single address
	if address, ok, err := unstructured.NestedString(spec, "address"); err == nil && ok {
		return port, protocol, tunnel, []string{address}, nil
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
		"Generate per-endpoint objects instead of a single object listing all service endpoints (requires -dry-run).")
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

	if err := checkFlags(*dryRun, *disableEndpointPooling); err != nil {
		log.Error(err, "invalid flags")
		os.Exit(1)
	}
	if *dryRun {
		log.Info("dry-run mode enabled")
	}

	// Define the controller pipeline
	specFile := SDWANOperatorGatherSpec
	if *disableEndpointPooling {
		specFile = SDWANOperatorSpec
	}

	ctx := ctrl.SetupSignalHandler()

	op, pc, err := NewOperator(specFile, logger)
	if err != nil {
		log.Error(err, "problem running operator")
		os.Exit(1)
	}

	if err := run(ctx, log, op.Start, server.Start, op.GetErrorChannel(), pc.waitChanges, changeTimeout); err != nil {
		log.Error(err, "problem running operator")
		os.Exit(1)
	}
}

// checkFlags rejects flag combinations the operator cannot run with.
func checkFlags(dryRun, disableEndpointPooling bool) error {
	// The SD-WAN manager keeps one list of all endpoints per service.
	if disableEndpointPooling && !dryRun {
		return errors.New("-disable-endpoint-pooling requires -dry-run: SD-WAN policies need endpoint pooling")
	}
	return nil
}

// run runs the operator and its API server until ctx is done or the API
// server fails. It then waits for the SD-WAN change in progress, if any, for at
// most timeout.
func run(ctx context.Context, log logr.Logger, startOp, startServer func(context.Context) error,
	errs <-chan error, waitChanges func(time.Duration) bool, timeout time.Duration) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go func() {
		err := startServer(ctx)
		if ctx.Err() != nil {
			return // shutting down
		}
		if err == nil {
			err = errors.New("stopped")
		}
		cancel(fmt.Errorf("API server: %w", err))
	}()

	// Create an error reporter thread
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errs:
				log.Error(err, "operator error")
			}
		}
	}()

	err := startOp(ctx)
	switch cause := context.Cause(ctx); {
	case cause == nil:
	case !errors.Is(cause, context.Canceled):
		err = errors.Join(err, cause)
	case err != nil:
		// Shutting down: stopping fails when a change outlasts the manager's
		// grace period, but the change is waited for below.
		log.Error(err, "operator did not stop cleanly")
		err = nil
	}
	if !waitChanges(timeout) {
		err = errors.Join(err, fmt.Errorf("SD-WAN change still in progress after %s", timeout))
	}

	return err
}
