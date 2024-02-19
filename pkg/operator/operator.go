// Package operator contains main implementation of Flatcar Linux Update Operator.
package operator

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/flatcar/flatcar-linux-update-operator/pkg/constants"
	"github.com/flatcar/flatcar-linux-update-operator/pkg/k8sutil"
)

const (
	leaderElectionEventSourceComponent = "update-operator-leader-election"
	defaultMaxRebootingNodes           = 1
	defaultLockType                    = resourcelock.LeasesResourceLock

	leaderElectionResourceName = "flatcar-linux-update-operator-lock"

	// Arbitrarily copied from KVO.
	defaultLeaderElectionLease = 90 * time.Second
	// ReconciliationPeriod.
	defaultReconciliationPeriod = 30 * time.Second
)

//nolint:godot // TODO: Complaining about not capitalized comments for variables. We should get rid of those completely.
var (
	// justRebootedSelector is a selector for combination of annotations
	// expected to be on a node after it has completed a reboot.
	//
	// The update-operator sets constants.AnnotationOkToReboot to true to
	// trigger a reboot, and the update-agent sets
	// constants.AnnotationRebootNeeded and
	// constants.AnnotationRebootInProgress to false when it has finished.
	justRebootedSelector = fields.Set(map[string]string{
		constants.AnnotationOkToReboot:       constants.True,
		constants.AnnotationRebootNeeded:     constants.False,
		constants.AnnotationRebootInProgress: constants.False,
	}).AsSelector()

	// rebootableSelector is a selector for the annotation expected to be on a node when it can be rebooted.
	//
	// The update-agent sets constants.AnnotationRebootNeeded to true when
	// it would like to reboot, and false when it starts up.
	//
	// If constants.AnnotationRebootPaused is set to "true", the update-agent will not consider it for rebooting.
	rebootableSelector = fields.ParseSelectorOrDie(constants.AnnotationRebootNeeded + "==" + constants.True +
		"," + constants.AnnotationRebootPaused + "!=" + constants.True +
		"," + constants.AnnotationOkToReboot + "!=" + constants.True +
		"," + constants.AnnotationRebootInProgress + "!=" + constants.True)

	// stillRebootingSelector is a selector for the annotation set expected to be
	// on a node when it's in the process of rebooting.
	stillRebootingSelector = fields.Set(map[string]string{
		constants.AnnotationOkToReboot:   constants.True,
		constants.AnnotationRebootNeeded: constants.True,
	}).AsSelector()

	// beforeRebootReq requires a node to be waiting for before reboot checks to complete.
	beforeRebootReq = k8sutil.NewRequirementOrDie(constants.LabelBeforeReboot, selection.In, []string{constants.True})

	// afterRebootReq requires a node to be waiting for after reboot checks to complete.
	afterRebootReq = k8sutil.NewRequirementOrDie(constants.LabelAfterReboot, selection.In, []string{constants.True})

	// notBeforeRebootReq is the inverse of the above checks.
	notBeforeRebootReq = k8sutil.NewRequirementOrDie(
		constants.LabelBeforeReboot, selection.NotIn, []string{constants.True})
)

// Config configures a Kontroller.
type Config struct {
	// Kubernetes client.
	Client kubernetes.Interface
	// Annotations to look for before and after reboots.
	BeforeRebootAnnotations []string
	AfterRebootAnnotations  []string
	// Reboot window.
	RebootWindowStart    string
	RebootWindowLength   string
	Namespace            string
	LockID               string
	LockType             string
	ReconciliationPeriod time.Duration
	LeaderElectionLease  time.Duration
	MaxRebootingNodes    int
}

// Kontroller implement operator part of FLUO.
type Kontroller struct {
	kc kubernetes.Interface
	nc corev1client.NodeInterface

	// Annotations to look for before and after reboots.
	beforeRebootAnnotations []string
	afterRebootAnnotations  []string

	// Namespace is the kubernetes namespace any resources (e.g. locks,
	// configmaps, agents) should be created and read under.
	// It will be set to the namespace the operator is running in automatically.
	namespace string

	// Reboot window.
	rebootWindow *Periodic

	maxRebootingNodes int

	reconciliationPeriod time.Duration

	leaderElectionLease time.Duration

	resourceLock resourcelock.Interface
}

// New initializes a new Kontroller.
func New(config Config) (*Kontroller, error) {
	if err := checkConfig(config); err != nil {
		return nil, fmt.Errorf("check configuration: %w", err)
	}

	resourceLock, err := newResourceLock(config)
	if err != nil {
		return nil, fmt.Errorf("creating new resource lock: %w", err)
	}

	var rebootWindow *Periodic

	if config.RebootWindowStart != "" && config.RebootWindowLength != "" {
		rw, err := ParsePeriodic(config.RebootWindowStart, config.RebootWindowLength)
		if err != nil {
			return nil, fmt.Errorf("parsing reboot window: %w", err)
		}

		rebootWindow = rw
	}

	reconciliationPeriod := config.ReconciliationPeriod
	if reconciliationPeriod == 0 {
		reconciliationPeriod = defaultReconciliationPeriod
	}

	leaderElectionLeaseDuration := config.LeaderElectionLease
	if leaderElectionLeaseDuration == 0 {
		leaderElectionLeaseDuration = defaultLeaderElectionLease
	}

	maxRebootingNodes := config.MaxRebootingNodes
	if maxRebootingNodes == 0 {
		maxRebootingNodes = defaultMaxRebootingNodes
	}

	return &Kontroller{
		kc:                      config.Client,
		nc:                      config.Client.CoreV1().Nodes(),
		beforeRebootAnnotations: config.BeforeRebootAnnotations,
		afterRebootAnnotations:  config.AfterRebootAnnotations,
		namespace:               config.Namespace,
		rebootWindow:            rebootWindow,
		maxRebootingNodes:       maxRebootingNodes,
		reconciliationPeriod:    reconciliationPeriod,
		leaderElectionLease:     leaderElectionLeaseDuration,
		resourceLock:            resourceLock,
	}, nil
}

// checkConfig checks a Kontroller configuration.
func checkConfig(config Config) error {
	// Kubernetes client.
	if config.Client == nil {
		return fmt.Errorf("kubernetes client must not be nil")
	}

	if config.Namespace == "" {
		return fmt.Errorf("namespace must not be empty")
	}

	if config.LockID == "" {
		return fmt.Errorf("lockID must not be empty")
	}

	return nil
}

// newResourceLock creates a resource for locking on arbitrary resources
// used in leader election.
func newResourceLock(config Config) (resourcelock.Interface, error) {
	lockType := config.LockType
	if lockType == "" {
		lockType = defaultLockType
	}

	leaderElectionBroadcaster := record.NewBroadcaster()
	leaderElectionBroadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{
		Interface: config.Client.CoreV1().Events(config.Namespace),
	})

	return resourcelock.New(
		lockType,
		config.Namespace,
		leaderElectionResourceName,
		config.Client.CoreV1(),
		config.Client.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity: config.LockID,
			EventRecorder: leaderElectionBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
				Component: leaderElectionEventSourceComponent,
			}),
		},
	)
}

// Run starts the operator reconcilitation process and runs until the stop
// channel is closed.
func (k *Kontroller) Run(stop <-chan struct{}) error {
	errCh := make(chan error, 1)

	// Leader election is responsible for shutting down the controller, so when leader election
	// is lost, controller is immediately stopped, as shared context will be cancelled.
	ctx := k.withLeaderElection(stop, errCh)

	klog.V(5).Info("Starting controller")

	// Call the process loop each period, until stop is closed.
	wait.Until(func() { k.process(ctx) }, k.reconciliationPeriod, ctx.Done())

	klog.V(5).Info("Stopping controller")

	return <-errCh
}

// withLeaderElection creates a new context which is cancelled when this
// operator does not hold a lock to operate on the cluster.
func (k *Kontroller) withLeaderElection(stop <-chan struct{}, errCh chan<- error) context.Context {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// When user requests to stop the controller, cancel context to interrupt any ongoing operation.
		<-stop
		errCh <- nil

		cancel()
	}()

	waitLeading := make(chan struct{})

	go func() {
		// Lease values inspired by a combination of
		// https://github.com/kubernetes/kubernetes/blob/f7c07a121d2afadde7aa15b12a9d02858b30a0a9/pkg/apis/componentconfig/v1alpha1/defaults.go#L163-L174
		// and the KVO values
		// See also
		// https://github.com/kubernetes/kubernetes/blob/fc31dae165f406026142f0dd9a98cada8474682a/pkg/client/leaderelection/leaderelection.go#L17
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:          k.resourceLock,
			LeaseDuration: k.leaderElectionLease,
			//nolint:gomnd // Set renew deadline to 2/3rd of the lease duration to give
			//             // controller enough time to renew the lease.
			RenewDeadline: k.leaderElectionLease * 2 / 3,
			//nolint:gomnd // Retry duration is usually around 1/10th of lease duration,
			//             // but given low dynamics of FLUO, 1/3rd should also be fine.
			RetryPeriod: k.leaderElectionLease / 3,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) { // was: func(stop <-chan struct{
					klog.V(5).Info("Started leading")
					waitLeading <- struct{}{}
				},
				OnStoppedLeading: func() {
					errCh <- fmt.Errorf("leaderelection lost")
					cancel()
				},
			},
		})
	}()

	<-waitLeading

	return ctx
}

// process performs the reconcilitation to coordinate reboots.
func (k *Kontroller) process(ctx context.Context) {
	klog.V(4).Info("Going through a loop cycle")

	// First make sure that all of our nodes are in a well-defined state with
	// respect to our annotations and labels, and if they are not, then try to
	// fix them.
	klog.V(4).Info("Cleaning up node state")

	if err := k.cleanupState(ctx); err != nil {
		klog.Errorf("Failed to cleanup node state: %v", err)

		return
	}

	// Find nodes with the after-reboot=true label and check if all provided
	// annotations are set. if all annotations are set to true then remove the
	// after-reboot=true label and set reboot-ok=false, telling the agent that
	// the reboot has completed.
	klog.V(4).Info("Checking if configured after-reboot annotations are set to true")

	if err := k.checkAfterReboot(ctx); err != nil {
		klog.Errorf("Failed to check after reboot: %v", err)

		return
	}

	// Find nodes which just rebooted but haven't run after-reboot checks.
	// remove after-reboot annotations and add the after-reboot=true label.
	klog.V(4).Info("Labeling rebooted nodes with after-reboot label")

	if err := k.markAfterReboot(ctx); err != nil {
		klog.Errorf("Failed to update recently rebooted nodes: %v", err)

		return
	}

	// Find nodes with the before-reboot=true label and check if all provided
	// annotations are set. if all annotations are set to true then remove the
	// before-reboot=true label and set reboot=ok=true, telling the agent it's
	// time to reboot.
	klog.V(4).Info("Checking if configured before-reboot annotations are set to true")

	if err := k.checkBeforeReboot(ctx); err != nil {
		klog.Errorf("Failed to check before reboot: %v", err)

		return
	}

	// Take some number of the rebootable nodes. remove before-reboot
	// annotations and add the before-reboot=true label.
	klog.V(4).Info("Labeling rebootable nodes with before-reboot label")

	if err := k.markBeforeReboot(ctx); err != nil {
		klog.Errorf("Failed to update rebootable nodes: %v", err)

		return
	}
}

// cleanupState attempts to make sure nodes are in a well-defined state before
// performing state changes on them.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) cleanupState(ctx context.Context) error {
	nodelist, err := k.nc.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	for _, node := range nodelist.Items {
		err = k8sutil.UpdateNodeRetry(ctx, k.nc, node.Name, func(node *corev1.Node) {
			// Make sure that nodes with the before-reboot label actually
			// still wants to reboot.
			if _, exists := node.Labels[constants.LabelBeforeReboot]; !exists {
				return
			}

			if rebootableSelector.Matches(fields.Set(node.Annotations)) {
				return
			}

			klog.Warningf("Node %q no longer wanted to reboot while we were trying to label it so: %v",
				node.Name, node.Annotations)
			delete(node.Labels, constants.LabelBeforeReboot)
			for _, annotation := range k.beforeRebootAnnotations {
				delete(node.Annotations, annotation)
			}
		})
		if err != nil {
			return fmt.Errorf("cleaning up node %q: %w", node.Name, err)
		}
	}

	return nil
}

type checkRebootOptions struct {
	req         *labels.Requirement
	annotations []string
	label       string
	okToReboot  string
}

// checkReboot gets all nodes with a given requirement and checks if all of the given annotations are set to true.
//
// If they are, it deletes given annotations and label, then sets ok-to-reboot annotation to either true or false,
// depending on the given parameter.
//
// If ok-to-reboot is set to true, it gives node agent a signal that it is OK to proceed with rebooting.
//
// If ok-to-reboot is set to false, it means node has finished rebooting successfully.
//
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) checkReboot(ctx context.Context, opt checkRebootOptions) error {
	nodelist, err := k.nc.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	nodes := k8sutil.FilterNodesByRequirement(nodelist.Items, opt.req)

	for _, node := range nodes {
		if !hasAllAnnotations(node, opt.annotations) {
			continue
		}

		klog.V(4).Infof("Deleting label %q for %q", opt.label, node.Name)
		klog.V(4).Infof("Setting annotation %q to %q for %q",
			constants.AnnotationOkToReboot, opt.okToReboot, node.Name)

		if err := k8sutil.UpdateNodeRetry(ctx, k.nc, node.Name, func(node *corev1.Node) {
			delete(node.Labels, opt.label)

			// Cleanup the annotations.
			for _, annotation := range opt.annotations {
				klog.V(4).Infof("Deleting annotation %q from node %q", annotation, node.Name)
				delete(node.Annotations, annotation)
			}

			node.Annotations[constants.AnnotationOkToReboot] = opt.okToReboot
		}); err != nil {
			return fmt.Errorf("updating node %q: %w", node.Name, err)
		}
	}

	return nil
}

// checkBeforeReboot gets all nodes with the before-reboot=true label and checks
// if all of the configured before-reboot annotations are set to true. If they
// are, it deletes the before-reboot=true label and sets reboot-ok=true to tell
// the agent that it is ready to start the actual reboot process.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) checkBeforeReboot(ctx context.Context) error {
	opt := checkRebootOptions{
		req:         beforeRebootReq,
		annotations: k.beforeRebootAnnotations,
		label:       constants.LabelBeforeReboot,
		okToReboot:  constants.True,
	}

	return k.checkReboot(ctx, opt)
}

// checkAfterReboot gets all nodes with the after-reboot=true label and checks
// if all of the configured after-reboot annotations are set to true. If they
// are, it deletes the after-reboot=true label and sets reboot-ok=false to tell
// the agent that it has completed it's reboot successfully.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) checkAfterReboot(ctx context.Context) error {
	opt := checkRebootOptions{
		req:         afterRebootReq,
		annotations: k.afterRebootAnnotations,
		label:       constants.LabelAfterReboot,
		okToReboot:  constants.False,
	}

	return k.checkReboot(ctx, opt)
}

// insideRebootWindow checks if process is inside reboot window at the time
// of calling this function.
//
// If reboot window is not configured, true is always returned.
func (k *Kontroller) insideRebootWindow() bool {
	if k.rebootWindow == nil {
		return true
	}

	// Most recent reboot window might still be open.
	mostRecentRebootWindow := k.rebootWindow.Previous(time.Now())

	return time.Now().Before(mostRecentRebootWindow.End)
}

// remainingRebootingCapacity calculates how many more nodes can be rebooted at a time based
// on a given list of nodes.
//
// If maximum capacity is reached, it is logged and list of rebooting nodes is logged as well.
func (k *Kontroller) remainingRebootingCapacity(nodelist *corev1.NodeList) int {
	rebootingNodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, stillRebootingSelector)

	// Nodes running before and after reboot checks are still considered to be "rebooting" to us.
	beforeRebootNodes := k8sutil.FilterNodesByRequirement(nodelist.Items, beforeRebootReq)
	afterRebootNodes := k8sutil.FilterNodesByRequirement(nodelist.Items, afterRebootReq)

	rebootingNodes = append(append(rebootingNodes, beforeRebootNodes...), afterRebootNodes...)

	remainingCapacity := k.maxRebootingNodes - len(rebootingNodes)

	if remainingCapacity == 0 {
		for _, n := range rebootingNodes {
			klog.Infof("Found node %q still rebooting, waiting", n.Name)
		}

		klog.Infof("Found %d (of max %d) rebooting nodes; waiting for completion", len(rebootingNodes), k.maxRebootingNodes)
	}

	return remainingCapacity
}

// nodesRequiringReboot filters given list of nodes and returns ones which requires a reboot.
func (k *Kontroller) nodesRequiringReboot(nodelist *corev1.NodeList) []corev1.Node {
	rebootableNodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, rebootableSelector)

	return k8sutil.FilterNodesByRequirement(rebootableNodes, notBeforeRebootReq)
}

// rebootableNodes returns list of nodes which can be marked for rebooting based on remaining capacity.
func (k *Kontroller) rebootableNodes(nodelist *corev1.NodeList) []*corev1.Node {
	remainingCapacity := k.remainingRebootingCapacity(nodelist)

	nodesRequiringReboot := k.nodesRequiringReboot(nodelist)

	chosenNodes := make([]*corev1.Node, 0, remainingCapacity)
	for i := 0; i < remainingCapacity && i < len(nodesRequiringReboot); i++ {
		chosenNodes = append(chosenNodes, &nodesRequiringReboot[i])
	}

	klog.Infof("Found %d nodes that need a reboot", len(chosenNodes))

	return chosenNodes
}

// markBeforeReboot gets nodes which want to reboot and marks them with the
// before-reboot=true label. This is considered the beginning of the reboot
// process from the perspective of the update-operator. It will only mark
// nodes with this label up to the maximum number of concurrently rebootable
// nodes as configured with the maxRebootingNodes constant. It also checks if
// we are inside the reboot window.
// It cleans up the before-reboot annotations before it applies the label, in
// case there are any left over from the last reboot.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) markBeforeReboot(ctx context.Context) error {
	nodelist, err := k.nc.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	if !k.insideRebootWindow() {
		klog.V(4).Info("We are outside the reboot window; not labeling rebootable nodes for now")

		return nil
	}

	// Set before-reboot=true for the chosen nodes.
	for _, n := range k.rebootableNodes(nodelist) {
		err = k.mark(ctx, n.Name, constants.LabelBeforeReboot, "before-reboot", k.beforeRebootAnnotations)
		if err != nil {
			return fmt.Errorf("labeling node for before reboot checks: %w", err)
		}
	}

	return nil
}

// markAfterReboot gets nodes which have completed rebooting and marks them with
// the after-reboot=true label. A node with the after-reboot=true label is still
// considered to be rebooting from the perspective of the update-operator, even
// though it has completed rebooting from the machines perspective.
// It cleans up the after-reboot annotations before it applies the label, in
// case there are any left over from the last reboot.
// If there is an error getting the list of nodes or updating any of them, an
// error is immediately returned.
func (k *Kontroller) markAfterReboot(ctx context.Context) error {
	nodelist, err := k.nc.List(ctx, metav1.ListOptions{
		// Filter out any nodes that are already labeled with after-reboot=true.
		LabelSelector: fmt.Sprintf("%s!=%s", constants.LabelAfterReboot, constants.True),
	})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	// Find nodes which just rebooted.
	justRebootedNodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, justRebootedSelector)

	klog.Infof("Found %d rebooted nodes", len(justRebootedNodes))

	// For all the nodes which just rebooted, remove any old annotations and add the after-reboot=true label.
	for _, n := range justRebootedNodes {
		err = k.mark(ctx, n.Name, constants.LabelAfterReboot, "after-reboot", k.afterRebootAnnotations)
		if err != nil {
			return fmt.Errorf("labeling node for after reboot checks: %w", err)
		}
	}

	return nil
}

func (k *Kontroller) mark(ctx context.Context, nodeName, label, annotationsType string, annotations []string) error {
	klog.V(4).Infof("Deleting annotations %v for %q", annotations, nodeName)
	klog.V(4).Infof("Setting label %q to %q for node %q", label, constants.True, nodeName)

	err := k8sutil.UpdateNodeRetry(ctx, k.nc, nodeName, func(node *corev1.Node) {
		for _, annotation := range annotations {
			delete(node.Annotations, annotation)
		}
		node.Labels[label] = constants.True
	})
	if err != nil {
		return fmt.Errorf("setting label %q to %q on node %q: %w", label, constants.True, nodeName, err)
	}

	if len(annotations) > 0 {
		klog.Infof("Waiting for %s annotations on node %q: %v", annotationsType, nodeName, annotations)
	}

	return nil
}

func hasAllAnnotations(node corev1.Node, annotations []string) bool {
	nodeAnnotations := node.GetAnnotations()

	for _, annotation := range annotations {
		value, ok := nodeAnnotations[annotation]
		if !ok || value != constants.True {
			return false
		}
	}

	return true
}
