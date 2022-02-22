package operator

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/flatcar-linux/locksmith/pkg/timeutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"

	"github.com/flatcar-linux/flatcar-linux-update-operator/pkg/constants"
)

const (
	testBeforeRebootAnnotation        = "test-before-annotation"
	testAnotherBeforeRebootAnnotation = "test-another-after-annotation"
	testAfterRebootAnnotation         = "test-after-annotation"
	testAnotherAfterRebootAnnotation  = "test-another-after-annotation"
	testNamespace                     = "default"
)

func Test_Operator_exits_gracefully_when_user_requests_shutdown(t *testing.T) {
	t.Parallel()

	rebootCancelledNode := rebootCancelledNode()

	config := testConfig(rebootCancelledNode)
	testKontroller := kontrollerWithObjects(t, config)
	testKontroller.reconciliationPeriod = 1 * time.Second

	stop := make(chan struct{})

	go func() {
		time.Sleep(testKontroller.reconciliationPeriod)
		close(stop)
	}()

	if err := testKontroller.Run(stop); err != nil {
		t.Fatalf("Unexpected run error: %v", err)
	}

	updatedNode := node(contextWithDeadline(t), t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
		t.Fatalf("Expected label %q to be removed from Node", constants.LabelBeforeReboot)
	}
}

//nolint:funlen
func Test_Operator_shuts_down_leader_election_process_when_user_requests_shutdown(t *testing.T) {
	t.Parallel()

	rebootCancelledNode := rebootCancelledNode()

	config := testConfig(rebootCancelledNode)
	config.BeforeRebootAnnotations = []string{testBeforeRebootAnnotation}
	testKontroller := kontrollerWithObjects(t, config)
	testKontroller.reconciliationPeriod = 1 * time.Second
	testKontroller.leaderElectionLease = 2 * time.Second

	stop := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		if err := testKontroller.Run(stop); err != nil {
			fmt.Printf("Error running operator: %v\n", err)
			t.Fail()
		}
		stopped <- struct{}{}
	}()

	// Wait for one reconciliation cycle to run.
	time.Sleep(testKontroller.reconciliationPeriod)

	ctx := contextWithDeadline(t)
	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
		t.Fatalf("Expected label %q to be removed from Node after waiting the reconciliation period",
			constants.LabelBeforeReboot)
	}

	close(stop)

	<-stopped

	updatedNode.Labels[constants.LabelBeforeReboot] = constants.True
	updatedNode.Annotations[testBeforeRebootAnnotation] = constants.True

	if _, err := config.Client.CoreV1().Nodes().Update(ctx, updatedNode, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Updating Node object: %v", err)
	}

	config.LockID = "bar"

	parallelKontroller := kontrollerWithObjects(t, config)
	parallelKontroller.reconciliationPeriod = testKontroller.reconciliationPeriod
	parallelKontroller.leaderElectionLease = testKontroller.leaderElectionLease

	stop = make(chan struct{})

	t.Cleanup(func() {
		close(stop)
	})

	go func() {
		if err := parallelKontroller.Run(stop); err != nil {
			fmt.Printf("Error running operator: %v\n", err)
			t.Fail()
		}
	}()

	time.Sleep(testKontroller.leaderElectionLease * 2)

	updatedNode = node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
		t.Fatalf("Expected label %q to be removed from Node after waiting the reconciliation period",
			constants.LabelBeforeReboot)
	}
}

func Test_Operator_emits_events_about_leader_election_to_configured_namespace(t *testing.T) {
	t.Parallel()

	config := testConfig()

	testController := kontrollerWithObjects(t, config)
	testController.reconciliationPeriod = time.Second

	stop := make(chan struct{})

	go func() {
		time.Sleep(testController.reconciliationPeriod)
		close(stop)
	}()

	if err := testController.Run(stop); err != nil {
		t.Fatalf("Unexpected run error: %v", err)
	}

	events, err := config.Client.CoreV1().Events(config.Namespace).List(contextWithDeadline(t), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed listing events: %v", err)
	}

	if len(events.Items) == 0 {
		t.Fatalf("Expected at least one event to be published")
	}
}

//nolint:funlen
func Test_Operator_returns_error_when_leadership_is_lost(t *testing.T) {
	t.Parallel()

	rebootCancelledNode := rebootCancelledNode()

	config := testConfig(rebootCancelledNode)
	config.BeforeRebootAnnotations = []string{testBeforeRebootAnnotation}
	testKontroller := kontrollerWithObjects(t, config)
	testKontroller.reconciliationPeriod = 1 * time.Second
	testKontroller.leaderElectionLease = 2 * time.Second

	stop := make(chan struct{})

	t.Cleanup(func() {
		close(stop)
	})

	errCh := make(chan error, 1)

	go func() {
		errCh <- testKontroller.Run(stop)
	}()

	// Wait for one reconciliation cycle to run.
	time.Sleep(testKontroller.reconciliationPeriod)

	ctx := contextWithDeadline(t)

	// Ensure operator is functional.
	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
		t.Fatalf("Expected label %q to be removed from Node after waiting the reconciliation period",
			constants.LabelBeforeReboot)
	}

	// Force-steal leader election.
	configMapClient := config.Client.CoreV1().ConfigMaps(config.Namespace)

	lock, err := configMapClient.Get(ctx, leaderElectionResourceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting lock ConfigMap %q: %v", leaderElectionResourceName, err)
	}

	leaderAnnotation := "control-plane.alpha.kubernetes.io/leader"

	leader, ok := lock.Annotations[leaderAnnotation]
	if !ok {
		t.Fatalf("expected annotation %q not found", leaderAnnotation)
	}

	leaderLease := &struct {
		HolderIdentity       string
		LeaseDurationSeconds int
		AcquireTime          time.Time
		RenewTime            time.Time
		LeaderTransitions    int
	}{}

	if err := json.Unmarshal([]byte(leader), leaderLease); err != nil {
		t.Fatalf("Decoding leader annotation data %q: %v", leader, err)
	}

	leaderLease.HolderIdentity = "baz"

	leaderBytes, err := json.Marshal(leaderLease)
	if err != nil {
		t.Fatalf("Encoding leader annotation data: %q: %v", leader, err)
	}

	lock.Annotations[leaderAnnotation] = string(leaderBytes)

	if _, err := configMapClient.Update(ctx, lock, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Updating lock ConfigMap %q: %v", leaderElectionResourceName, err)
	}

	// Wait lease time to ensure operator lost it.
	time.Sleep(testKontroller.leaderElectionLease)

	// Patch node object again to verify if operator is functional.
	updatedNode.Labels[constants.LabelBeforeReboot] = constants.True
	updatedNode.Annotations[testBeforeRebootAnnotation] = constants.True

	if _, err := config.Client.CoreV1().Nodes().Update(ctx, updatedNode, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Updating Node object: %v", err)
	}

	time.Sleep(testKontroller.reconciliationPeriod)

	updatedNode = node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; !ok {
		t.Fatalf("Expected label %q to remain on Node", constants.LabelBeforeReboot)
	}

	if err := <-errCh; err == nil {
		t.Fatalf("Expected operator to return error when leader election is lost")
	}
}

//nolint:funlen
func Test_Operator_waits_for_leader_election_before_reconciliation(t *testing.T) {
	t.Parallel()

	rebootCancelledNode := rebootCancelledNode()

	config := testConfig(rebootCancelledNode)
	config.BeforeRebootAnnotations = []string{testBeforeRebootAnnotation}
	testKontroller := kontrollerWithObjects(t, config)
	testKontroller.reconciliationPeriod = 1 * time.Second

	stop := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		if err := testKontroller.Run(stop); err != nil {
			fmt.Printf("Error running operator: %v\n", err)
			t.Fail()
		}
		stopped <- struct{}{}
	}()

	time.Sleep(testKontroller.reconciliationPeriod)

	close(stop)

	<-stopped

	ctx := contextWithDeadline(t)
	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
		t.Fatalf("Expected label %q to be removed from Node after waiting the reconciliation period",
			constants.LabelBeforeReboot)
	}

	updatedNode.Labels[constants.LabelBeforeReboot] = constants.True
	updatedNode.Annotations[testBeforeRebootAnnotation] = constants.True

	if _, err := config.Client.CoreV1().Nodes().Update(ctx, updatedNode, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Updating Node object: %v", err)
	}

	config.LockID = "bar"
	parallelKontroller := kontrollerWithObjects(t, config)
	parallelKontroller.reconciliationPeriod = testKontroller.reconciliationPeriod

	stop = make(chan struct{})

	t.Cleanup(func() {
		close(stop)
	})

	runOperator(t, parallelKontroller, stop)

	time.Sleep(parallelKontroller.reconciliationPeriod)

	updatedNode = node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; !ok {
		t.Fatalf("Expected label %q to remain on Node", constants.LabelBeforeReboot)
	}
}

func Test_Operator_stops_reconciliation_loop_when_control_channel_is_closed(t *testing.T) {
	t.Parallel()

	rebootCancelledNode := rebootCancelledNode()

	config := testConfig(rebootCancelledNode)
	config.BeforeRebootAnnotations = []string{testBeforeRebootAnnotation}
	testKontroller := kontrollerWithObjects(t, config)
	testKontroller.reconciliationPeriod = 1 * time.Second

	stop := make(chan struct{})

	runOperator(t, testKontroller, stop)

	time.Sleep(testKontroller.reconciliationPeriod)

	ctx := contextWithDeadline(t)
	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
		t.Fatalf("Expected label %q to be removed from Node after waiting the reconciliation period",
			constants.LabelBeforeReboot)
	}

	close(stop)

	time.Sleep(testKontroller.reconciliationPeriod * 2)

	updatedNode.Labels[constants.LabelBeforeReboot] = constants.True
	updatedNode.Annotations[testBeforeRebootAnnotation] = constants.True

	if _, err := config.Client.CoreV1().Nodes().Update(ctx, updatedNode, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Updating Node object: %v", err)
	}

	time.Sleep(testKontroller.reconciliationPeriod * 2)

	updatedNode = node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; !ok {
		t.Fatalf("Expected label %q to remain on Node", constants.LabelBeforeReboot)
	}
}

func Test_Operator_reconciles_objects_every_configured_period(t *testing.T) {
	t.Parallel()

	rebootCancelledNode := rebootCancelledNode()

	config := testConfig(rebootCancelledNode)
	config.BeforeRebootAnnotations = []string{testBeforeRebootAnnotation}
	testKontroller := kontrollerWithObjects(t, config)
	testKontroller.reconciliationPeriod = 1 * time.Second

	stop := make(chan struct{})

	t.Cleanup(func() {
		close(stop)
	})

	runOperator(t, testKontroller, stop)

	time.Sleep(testKontroller.reconciliationPeriod)

	ctx := contextWithDeadline(t)
	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
		t.Fatalf("Expected label %q to be removed from Node after waiting the reconciliation period",
			constants.LabelBeforeReboot)
	}

	updatedNode.Labels[constants.LabelBeforeReboot] = constants.True
	updatedNode.Annotations[testBeforeRebootAnnotation] = constants.True

	if _, err := config.Client.CoreV1().Nodes().Update(ctx, updatedNode, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Updating Node object: %v", err)
	}

	time.Sleep(testKontroller.reconciliationPeriod * 2)

	updatedNode = node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
		t.Fatalf("Expected label %q to be removed from Node after waiting another reconciliation period",
			constants.LabelBeforeReboot)
	}
}

// before-reboot label is intended to be used as a selector for pre-reboot hooks, so it should only
// be set for nodes, which are ready to start rebooting any minute.
//
//nolint:funlen
func Test_Operator_cleans_up_nodes_which_cannot_be_rebooted(t *testing.T) {
	t.Parallel()

	rebootCancelledNode := rebootCancelledNode()

	toBeRebootedNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bar",
			Annotations: map[string]string{
				testBeforeRebootAnnotation: "",
			},
		},
	}

	config := testConfig(rebootCancelledNode, toBeRebootedNode)
	config.BeforeRebootAnnotations = []string{testBeforeRebootAnnotation}
	testKontroller := kontrollerWithObjects(t, config)

	ctx := contextWithDeadline(t)
	testKontroller.process(ctx)

	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootCancelledNode.Name)

	t.Run("by", func(t *testing.T) {
		t.Parallel()

		t.Run("removing_before_reboot_label", func(t *testing.T) {
			t.Parallel()

			if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
				t.Fatalf("Unexpected label %q found", constants.LabelBeforeReboot)
			}
		})

		t.Run("removing_configured_before_reboot_annotations", func(t *testing.T) {
			t.Parallel()

			if _, ok := updatedNode.Annotations[testBeforeRebootAnnotation]; ok {
				t.Fatalf("Unexpected annotation %q found for node %q", testBeforeRebootAnnotation, rebootCancelledNode.Name)
			}

			updatedToBeRebootedNode := node(ctx, t, config.Client.CoreV1().Nodes(), toBeRebootedNode.Name)

			if _, ok := updatedToBeRebootedNode.Annotations[testBeforeRebootAnnotation]; !ok {
				t.Fatalf("Annotation %q has been removed from wrong node %q",
					testBeforeRebootAnnotation, toBeRebootedNode.Name)
			}
		})
	})

	// To avoid rebooting nodes which executed before-reboot hooks, but don't need a reboot anymore.
	t.Run("before_reboot_is_approved", func(t *testing.T) {
		t.Parallel()

		if v, ok := updatedNode.Annotations[constants.AnnotationOkToReboot]; ok && v == "true" {
			t.Fatalf("Unexpected reboot approval")
		}
	})
}

func Test_Operator_does_not_count_nodes_as_rebooting_which(t *testing.T) {
	t.Parallel()

	ctx := contextWithDeadline(t)

	cases := map[string]*corev1.Node{
		"has_finished_rebooting": finishedRebootingNode(),
		"are_idle":               idleNode(),
	}

	for name, c := range cases { //nolint:paralleltest
		c := c

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rebootableNode := rebootableNode()

			extraNode := c

			config := testConfig(extraNode, rebootableNode)
			testKontroller := kontrollerWithObjects(t, config)

			testKontroller.process(ctx)

			updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootableNode.Name)

			beforeReboot, ok := updatedNode.Labels[constants.LabelBeforeReboot]
			if !ok {
				t.Fatalf("Expected label %q", constants.LabelBeforeReboot)
			}

			if beforeReboot != constants.True {
				t.Fatalf("Expected value %q for label %q, got: %q", constants.True, constants.LabelBeforeReboot, beforeReboot)
			}
		})
	}
}

// This test attempts to schedule a reboot for a schedulable node and depending on the
// state of the rebooting node, controller will either proceed with scheduling or will
// not do anything.
func Test_Operator_counts_nodes_as_rebooting_which(t *testing.T) {
	t.Parallel()

	ctx := contextWithDeadline(t)

	cases := map[string]*corev1.Node{
		"are_scheduled_for_reboot_already": scheduledForRebootNode(),
		"are_ready_to_reboot":              readyToRebootNode(),
		"has_reboot_approved":              rebootNotConfirmedNode(),
		"are_rebooting":                    rebootingNode(),
		"just_rebooted":                    justRebootedNode(),
	}

	for name, c := range cases { //nolint:paralleltest
		c := c

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rebootableNode := rebootableNode()

			extraNode := c

			config := testConfig(extraNode, rebootableNode)

			// Required to test selecting rebooting nodes only with before-reboot label, otherwise
			// it gets removed before we schedule nodes for rebooting.
			config.BeforeRebootAnnotations = []string{testBeforeRebootAnnotation}

			testKontroller := kontrollerWithObjects(t, config)

			testKontroller.process(ctx)

			updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootableNode.Name)

			if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
				t.Fatalf("Unexpected node %q scheduled for rebooting", rebootableNode.Name)
			}
		})
	}
}

func Test_Operator_does_not_count_nodes_as_rebootable_which(t *testing.T) {
	t.Parallel()

	ctx := contextWithDeadline(t)

	cases := map[string]func(*corev1.Node){
		"do_not_require_reboot": func(updatedNode *corev1.Node) {
			updatedNode.Annotations[constants.AnnotationRebootNeeded] = constants.False
		},
		"are_already_rebooting": func(updatedNode *corev1.Node) {
			*updatedNode = *rebootingNode()
			updatedNode.Annotations[testBeforeRebootAnnotation] = constants.True
			updatedNode.Annotations[testAnotherBeforeRebootAnnotation] = constants.True
		},
		"has_reboot_paused": func(updatedNode *corev1.Node) {
			updatedNode.Annotations[constants.AnnotationRebootPaused] = constants.True
		},
		"has_reboot_already_scheduled": func(updatedNode *corev1.Node) {
			updatedNode.Labels[constants.LabelBeforeReboot] = constants.True
			updatedNode.Annotations[testAnotherBeforeRebootAnnotation] = constants.False
		},
	}

	for name, c := range cases { //nolint:paralleltest
		c := c

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rebootableNode := rebootableNode()
			rebootableNode.Annotations[testBeforeRebootAnnotation] = constants.True
			rebootableNode.Annotations[testAnotherBeforeRebootAnnotation] = constants.True

			c(rebootableNode)

			config := testConfig(rebootableNode)
			testKontroller := kontrollerWithObjects(t, config)

			// To test filter on before-reboot label.
			testKontroller.maxRebootingNodes = 2
			testKontroller.beforeRebootAnnotations = []string{testBeforeRebootAnnotation, testAnotherBeforeRebootAnnotation}

			testKontroller.process(ctx)

			updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootableNode.Name)

			if _, ok := updatedNode.Annotations[testBeforeRebootAnnotation]; !ok {
				t.Fatalf("Unexpected node %q scheduled for rebooting", rebootableNode.Name)
			}
		})
	}
}

func Test_Operator_counts_nodes_as_rebootable_which_needs_reboot_and_has_all_other_conditions_met(t *testing.T) {
	t.Parallel()

	rebootableNode := rebootableNode()

	config := testConfig(rebootableNode)
	testKontroller := kontrollerWithObjects(t, config)

	ctx := contextWithDeadline(t)
	testKontroller.process(ctx)

	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootableNode.Name)

	v, ok := updatedNode.Labels[constants.LabelBeforeReboot]
	if !ok || v != constants.True {
		t.Fatalf("Expected node %q to be scheduled for rebooting", rebootableNode.Name)
	}
}

func Test_Operator_does_not_schedules_reboot_process_outside_reboot_window(t *testing.T) {
	t.Parallel()

	rebootableNode := rebootableNode()

	config := testConfig(rebootableNode)
	config.RebootWindowStart = "Mon 14:00"
	config.RebootWindowLength = "0s"

	testKontroller := kontrollerWithObjects(t, config)

	ctx := contextWithDeadline(t)

	testKontroller.process(ctx)

	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootableNode.Name)
	if v, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok && v == constants.True {
		t.Fatalf("Unexpected node %q scheduled for reboot", rebootableNode.Name)
	}
}

// To schedule pre-reboot hooks.
//
//nolint:funlen
func Test_Operator_schedules_reboot_process(t *testing.T) {
	t.Parallel()

	ctx := contextWithDeadline(t)

	t.Run("only_during_reboot_window", func(t *testing.T) {
		t.Parallel()

		rebootableNode := rebootableNode()

		config := testConfig(rebootableNode)
		testKontroller := kontrollerWithObjects(t, config)

		rw, err := timeutil.ParsePeriodic("Mon 00:00", fmt.Sprintf("%ds", (7*24*60*60)-1))
		if err != nil {
			t.Fatalf("Parsing reboot window: %v", err)
		}

		testKontroller.rebootWindow = rw

		testKontroller.process(ctx)

		updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootableNode.Name)
		if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; !ok {
			t.Fatalf("Expected node %q to be scheduled for reboot", rebootableNode.Name)
		}
	})

	t.Run("only_for_maximum_number_of_rebooting_nodes_in_parallel", func(t *testing.T) {
		t.Parallel()

		rebootableNode := rebootableNode()

		config := testConfig(rebootableNode, rebootNotConfirmedNode())

		testKontroller := kontrollerWithObjects(t, config)

		testKontroller.process(ctx)

		updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootableNode.Name)
		if v, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok && v == constants.True {
			t.Fatalf("Unexpected node %q scheduled for reboot", rebootableNode.Name)
		}
	})

	t.Run("for_nodes_which_are_rebootable", func(t *testing.T) {
		t.Parallel()

		scheduledForRebootNode := scheduledForRebootNode()

		config := testConfig(scheduledForRebootNode)

		testKontroller := kontrollerWithObjects(t, config)

		testKontroller.process(ctx)

		updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), scheduledForRebootNode.Name)

		if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
			t.Fatalf("Unexpected node %q scheduled for reboot", updatedNode.Name)
		}
	})

	t.Run("by", func(t *testing.T) {
		t.Parallel()

		rebootableNode := rebootableNode()
		rebootableNode.Annotations[testBeforeRebootAnnotation] = constants.True

		config := testConfig(rebootableNode)
		config.BeforeRebootAnnotations = []string{testBeforeRebootAnnotation}
		testKontroller := kontrollerWithObjects(t, config)

		testKontroller.process(ctx)

		updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), rebootableNode.Name)

		t.Run("removing_all_before_reboot_annotations", func(t *testing.T) {
			t.Parallel()

			if _, ok := updatedNode.Annotations[testBeforeRebootAnnotation]; ok {
				t.Fatalf("Unexpected annotation %q found", testBeforeRebootAnnotation)
			}
		})

		t.Run("setting_before_reboot_label_to_true", func(t *testing.T) {
			t.Parallel()

			beforeReboot, ok := updatedNode.Labels[constants.LabelBeforeReboot]
			if !ok {
				t.Fatalf("Expected label %q not found, got %v instead", constants.LabelBeforeReboot, updatedNode.Labels)
			}

			if beforeReboot != constants.True {
				t.Fatalf("Unexpected label value: %q", beforeReboot)
			}
		})
	})
}

func Test_Operator_approves_reboot_process_for_nodes_which_have(t *testing.T) {
	t.Parallel()

	ctx := contextWithDeadline(t)

	cases := map[string]struct {
		mutateF        func(*corev1.Node)
		expectRebootOK bool
	}{
		"all_conditions_met": {
			// Node without mutation should get ok-to-reboot.
			expectRebootOK: true,
		},
		"before_reboot_label": {
			mutateF: func(updatedNode *corev1.Node) {
				// Node without before-reboot label won't get ok-to-reboot.
				delete(updatedNode.Labels, constants.LabelBeforeReboot)
			},
		},
		"all_before_reboot_annotations_set_to_true": {
			mutateF: func(updatedNode *corev1.Node) {
				// Node without all before reboot annotations won't get ok-to-reboot.
				updatedNode.Annotations[testBeforeRebootAnnotation] = constants.False
			},
		},
	}

	for name, c := range cases { //nolint:paralleltest
		c := c

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			readyToRebootNode := readyToRebootNode()
			if c.mutateF != nil {
				c.mutateF(readyToRebootNode)
			}

			config := testConfig(readyToRebootNode)
			testKontroller := kontrollerWithObjects(t, config)
			// Use beforeRebootAnnotations to be able to test moment when node has before-reboot
			// label, but it cannot be removed yet.
			testKontroller.beforeRebootAnnotations = []string{testBeforeRebootAnnotation}

			testKontroller.process(ctx)

			updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), readyToRebootNode.Name)

			v, ok := updatedNode.Annotations[constants.AnnotationOkToReboot]
			if c.expectRebootOK && (!ok || v != constants.True) {
				t.Fatalf("Expected reboot-ok annotation, got %v", updatedNode.Annotations)
			}

			if !c.expectRebootOK && ok && v == constants.True {
				t.Fatalf("Unexpected reboot-ok annotation")
			}
		})
	}
}

// To inform agent it can proceed with node draining and rebooting.
func Test_Operator_approves_reboot_process_by(t *testing.T) {
	t.Parallel()

	readyToRebootNode := readyToRebootNode()

	config := testConfig(readyToRebootNode)
	config.BeforeRebootAnnotations = []string{testBeforeRebootAnnotation}
	testKontroller := kontrollerWithObjects(t, config)

	ctx := contextWithDeadline(t)

	testKontroller.process(ctx)

	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), readyToRebootNode.Name)

	// To de-schedule hook pods.
	t.Run("removing_before_reboot_label", func(t *testing.T) {
		t.Parallel()

		if _, ok := updatedNode.Labels[constants.LabelBeforeReboot]; ok {
			t.Fatalf("Unexpected label %q found", constants.LabelBeforeReboot)
		}
	})

	t.Run("removing_all_before_reboot_annotations", func(t *testing.T) {
		t.Parallel()

		if _, ok := updatedNode.Annotations[testBeforeRebootAnnotation]; ok {
			t.Fatalf("Unexpected annotation %q found", testBeforeRebootAnnotation)
		}
	})

	// To inform agent that all hooks are executed and it can proceed with the reboot.
	// Right now by setting ok-to-reboot label to true.
	t.Run("informing_agent_to_proceed_with_reboot_process", func(t *testing.T) {
		t.Parallel()

		okToReboot, ok := updatedNode.Annotations[constants.AnnotationOkToReboot]

		if !ok {
			t.Fatalf("Expected annotation %q not found, got %v", constants.AnnotationOkToReboot, updatedNode.Annotations)
		}

		if okToReboot != constants.True {
			t.Fatalf("Expected annotation %q value to be %q, got %q",
				constants.AnnotationOkToReboot, constants.True, okToReboot)
		}
	})
}

// Test opposite conditions starting from base to make sure all cases are covered.
//
//nolint:funlen,cyclop
func Test_Operator_counts_nodes_as_just_rebooted_which(t *testing.T) {
	t.Parallel()

	ctx := contextWithDeadline(t)

	cases := map[string]struct {
		mutateF            func(*corev1.Node)
		expectJustRebooted bool
	}{
		"has_all_conditions_met": {
			expectJustRebooted: true,
		},
		// Nodes which we allowed to reboot.
		"has_reboot_approved": {
			mutateF: func(updatedNode *corev1.Node) {
				updatedNode.Annotations[constants.AnnotationOkToReboot] = constants.False
			},
		},
		// Nodes which already rebooted.
		"does_not_need_a_reboot": {
			mutateF: func(updatedNode *corev1.Node) {
				updatedNode.Annotations[constants.AnnotationRebootNeeded] = constants.True
			},
		},
		// Nodes which already reported that they are back from rebooting.
		"which_finished_the_reboot": {
			mutateF: func(updatedNode *corev1.Node) {
				updatedNode.Annotations[constants.AnnotationRebootInProgress] = constants.True
			},
		},
		// Nodes which do not have hooks scheduled yet.
		"has_no_after_reboot_label": {
			mutateF: func(updatedNode *corev1.Node) {
				updatedNode.Labels[constants.LabelAfterReboot] = constants.True
			},
		},
	}

	for name, c := range cases { //nolint:paralleltest
		c := c

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			justRebootedNode := justRebootedNode()
			if c.mutateF != nil {
				c.mutateF(justRebootedNode)
			}

			config := testConfig(justRebootedNode)
			config.AfterRebootAnnotations = []string{testAfterRebootAnnotation, testAnotherAfterRebootAnnotation}

			testKontroller := kontrollerWithObjects(t, config)

			testKontroller.process(ctx)

			updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), justRebootedNode.Name)

			v, ok := updatedNode.Labels[constants.LabelAfterReboot]
			if c.expectJustRebooted {
				if !ok || v != constants.True {
					t.Errorf("Expected after reboot label, got %v", updatedNode.Labels)
				}

				if _, ok := updatedNode.Annotations[testAfterRebootAnnotation]; ok {
					t.Errorf("Expected annotation %q to be removed", testAfterRebootAnnotation)
				}

				if _, ok := updatedNode.Annotations[testAnotherAfterRebootAnnotation]; ok {
					t.Errorf("Expected annotation %q to be removed", testAnotherAfterRebootAnnotation)
				}
			}

			if !c.expectJustRebooted {
				v, ok := updatedNode.Annotations[testAfterRebootAnnotation]
				if !ok || v != constants.False {
					t.Fatalf("Expected annotation %q to be left untouched", testAfterRebootAnnotation)
				}
			}
		})
	}
}

// To schedule post-reboot hooks.
func Test_Operator_confirms_reboot_process_by(t *testing.T) {
	t.Parallel()

	justRebootedNode := justRebootedNode()
	justRebootedNode.Annotations[testAfterRebootAnnotation] = constants.True
	justRebootedNode.Annotations[testAnotherAfterRebootAnnotation] = constants.True

	config := testConfig(justRebootedNode)
	config.AfterRebootAnnotations = []string{testAfterRebootAnnotation, testAnotherAfterRebootAnnotation}
	testKontroller := kontrollerWithObjects(t, config)

	ctx := contextWithDeadline(t)

	testKontroller.process(ctx)

	updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), justRebootedNode.Name)

	// To ensure all annotations are freshly set.
	t.Run("removing_all_after_reboot_annotations", func(t *testing.T) {
		t.Parallel()

		if _, ok := updatedNode.Annotations[testAfterRebootAnnotation]; ok {
			t.Fatalf("Unexpected annotation %q found", testAfterRebootAnnotation)
		}

		if _, ok := updatedNode.Annotations[testAnotherAfterRebootAnnotation]; ok {
			t.Fatalf("Unexpected annotation %q found", testAnotherAfterRebootAnnotation)
		}
	})

	// To schedule after-reboot hook pods.
	t.Run("setting_after_reboot_label_to_true", func(t *testing.T) {
		t.Parallel()

		afterReboot, ok := updatedNode.Labels[constants.LabelAfterReboot]
		if !ok {
			t.Fatalf("Expected label %q not found, not %v", constants.LabelAfterReboot, updatedNode.Labels)
		}

		if afterReboot != constants.True {
			t.Fatalf("Expected label value %q, got %q", constants.True, afterReboot)
		}
	})
}

// Test opposite conditions starting from base to make sure all cases are covered.
func Test_Operator_counts_nodes_as_which_finished_rebooting_which_has(t *testing.T) {
	t.Parallel()

	ctx := contextWithDeadline(t)

	cases := map[string]struct {
		mutateF                 func(*corev1.Node)
		expectFinishedRebooting bool
	}{
		"all_conditions_met": {
			expectFinishedRebooting: true,
		},
		// Only consider nodes which runs the after-reboot hooks.
		"after_reboot_label_set": {
			mutateF: func(updatedNode *corev1.Node) {
				delete(updatedNode.Labels, constants.LabelAfterReboot)
			},
		},
		// To verify all hooks executed successfully.
		"all_after_reboot_annotations_set_to_true": {
			mutateF: func(updatedNode *corev1.Node) {
				updatedNode.Annotations[testAfterRebootAnnotation] = constants.False
			},
		},
	}

	for name, c := range cases { //nolint:paralleltest
		c := c

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			finishedRebootingNode := finishedRebootingNode()
			if c.mutateF != nil {
				c.mutateF(finishedRebootingNode)
			}

			config := testConfig(finishedRebootingNode)
			config.AfterRebootAnnotations = []string{testAfterRebootAnnotation, testAnotherAfterRebootAnnotation}
			testKontroller := kontrollerWithObjects(t, config)

			testKontroller.process(ctx)

			updatedNode := node(ctx, t, config.Client.CoreV1().Nodes(), finishedRebootingNode.Name)

			v, ok := updatedNode.Annotations[constants.AnnotationOkToReboot]
			if !c.expectFinishedRebooting && ok && v != constants.True {
				t.Fatalf("Expected after reboot label, got %v", updatedNode.Labels)
			}

			if c.expectFinishedRebooting && ok && v == constants.True {
				t.Fatalf("Unexpected after reboot label")
			}
		})
	}
}

// To de-schedule post-reboot hooks.
func Test_Operator_finishes_reboot_process_by(t *testing.T) {
	t.Parallel()

	finishedRebootingNode := finishedRebootingNode()

	config := testConfig(finishedRebootingNode)
	testKontroller := kontrollerWithObjects(t, config)
	testKontroller.afterRebootAnnotations = []string{testAfterRebootAnnotation, testAnotherAfterRebootAnnotation}

	testKontroller.process(contextWithDeadline(t))

	updatedNode := node(contextWithDeadline(t), t, config.Client.CoreV1().Nodes(), finishedRebootingNode.Name)

	// To de-schedule hook pods.
	t.Run("removing_after_reboot_label", func(t *testing.T) {
		t.Parallel()

		if _, ok := updatedNode.Labels[constants.LabelAfterReboot]; ok {
			t.Fatalf("Unexpected after reboot label found")
		}
	})

	// To cleanup the state before next runs.
	t.Run("removing_all_after_reboot_annotations", func(t *testing.T) {
		t.Parallel()

		if _, ok := updatedNode.Annotations[testAfterRebootAnnotation]; ok {
			t.Fatalf("Unexpected after reboot annotation %q found", testAfterRebootAnnotation)
		}

		if _, ok := updatedNode.Annotations[testAnotherAfterRebootAnnotation]; ok {
			t.Fatalf("Unexpected after reboot annotation %q found", testAnotherAfterRebootAnnotation)
		}
	})

	// To finalize reboot process. Implementation detail! setting ok-to-reboot label to false.
	t.Run("informing_agent_to_not_proceed_with_reboot_process", func(t *testing.T) {
		t.Parallel()

		okToReboot, ok := updatedNode.Annotations[constants.AnnotationOkToReboot]
		if !ok {
			t.Fatalf("Expected annotation %q not found, got %v", constants.AnnotationOkToReboot, updatedNode.Labels)
		}

		if okToReboot != constants.False {
			t.Fatalf("Expected annotation %q value %q, got %q", constants.AnnotationOkToReboot, constants.False, okToReboot)
		}
	})
}

// Expose klog flags to be able to increase verbosity for operator logs.
func TestMain(m *testing.M) {
	testFlags := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	klog.InitFlags(testFlags)

	if err := testFlags.Parse([]string{"-v=10"}); err != nil {
		fmt.Printf("Failed parsing flags: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func contextWithDeadline(t *testing.T) context.Context {
	t.Helper()

	deadline, ok := t.Deadline()
	if !ok {
		return context.Background()
	}

	// Arbitrary amount of time to let tests exit cleanly before main process terminates.
	timeoutGracePeriod := 10 * time.Second

	ctx, cancel := context.WithDeadline(context.Background(), deadline.Truncate(timeoutGracePeriod))
	t.Cleanup(cancel)

	return ctx
}

func runOperator(t *testing.T, k *Kontroller, stopCh <-chan struct{}) {
	t.Helper()

	go func() {
		if err := k.Run(stopCh); err != nil {
			fmt.Printf("Error running operator: %v\n", err)
			t.Fail()
		}
	}()
}

func testConfig(objects ...runtime.Object) Config {
	return Config{
		Client:    fake.NewSimpleClientset(objects...),
		LockID:    "foo",
		Namespace: testNamespace,
	}
}

func kontrollerWithObjects(t *testing.T, config Config) *Kontroller {
	t.Helper()

	kontroller, err := New(config)
	if err != nil {
		t.Fatalf("Failed creating controller instance: %v", err)
	}

	kontroller.reconciliationPeriod = defaultReconciliationPeriod
	kontroller.leaderElectionLease = defaultLeaderElectionLease
	kontroller.maxRebootingNodes = 1

	return kontroller
}

// Node with no need for rebooting.
func idleNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "idle",
			Labels: map[string]string{},
			Annotations: map[string]string{
				constants.AnnotationOkToReboot:       constants.False,
				constants.AnnotationRebootNeeded:     constants.False,
				constants.AnnotationRebootInProgress: constants.False,
			},
		},
	}
}

// Node with need for rebooting.
func rebootableNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rebootable",
			Labels: map[string]string{
				constants.LabelRebootNeeded: constants.True,
			},
			Annotations: map[string]string{
				constants.AnnotationRebootNeeded:     constants.True,
				constants.AnnotationOkToReboot:       constants.False,
				constants.AnnotationRebootInProgress: constants.False,
				testBeforeRebootAnnotation:           constants.False,
			},
		},
	}
}

// Node which has been scheduled for rebooting and runs before reboot hooks.
func scheduledForRebootNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scheduled-for-reboot",
			Labels: map[string]string{
				constants.LabelBeforeReboot: constants.True,
			},
			Annotations: map[string]string{
				constants.AnnotationRebootNeeded:     constants.True,
				constants.AnnotationOkToReboot:       constants.False,
				constants.AnnotationRebootInProgress: constants.False,
			},
		},
	}
}

// Node which has run pre-reboot hooks, but no longer needs a reboot.
func rebootCancelledNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "before-reboot",
			Labels: map[string]string{
				constants.LabelBeforeReboot: constants.True,
			},
			Annotations: map[string]string{
				testBeforeRebootAnnotation: constants.True,
			},
		},
	}
}

// Node which has finished running before reboot hooks.
func readyToRebootNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ready-to-reboot",
			Labels: map[string]string{
				constants.LabelBeforeReboot: constants.True,
			},
			Annotations: map[string]string{
				constants.AnnotationRebootNeeded:     constants.True,
				testBeforeRebootAnnotation:           constants.True,
				constants.AnnotationOkToReboot:       constants.False,
				constants.AnnotationRebootInProgress: constants.False,
			},
		},
	}
}

// Node which reboot has been approved by operator, but not confirmed by agent.
func rebootNotConfirmedNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "reboot-not-confirmed",
			Labels: map[string]string{},
			Annotations: map[string]string{
				constants.AnnotationOkToReboot:       constants.True,
				constants.AnnotationRebootNeeded:     constants.True,
				constants.AnnotationRebootInProgress: constants.False,
			},
		},
	}
}

// Node which reboot has been confirmed by agent.
func rebootingNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "rebooting",
			Labels: map[string]string{},
			Annotations: map[string]string{
				constants.AnnotationOkToReboot:       constants.True,
				constants.AnnotationRebootNeeded:     constants.True,
				constants.AnnotationRebootInProgress: constants.True,
			},
		},
	}
}

// Node which agent just finished rebooting.
func justRebootedNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "just-rebooted",
			Labels: map[string]string{},
			Annotations: map[string]string{
				constants.AnnotationOkToReboot:       constants.True,
				constants.AnnotationRebootNeeded:     constants.False,
				constants.AnnotationRebootInProgress: constants.False,

				// Test data.
				testAfterRebootAnnotation:        constants.False,
				testAnotherAfterRebootAnnotation: constants.False,
			},
		},
	}
}

// Node which runs after reboot hooks.
func finishedRebootingNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "finished-rebooting",
			Labels: map[string]string{
				constants.LabelAfterReboot: constants.True,
			},
			Annotations: map[string]string{
				constants.AnnotationOkToReboot:       constants.True,
				testAfterRebootAnnotation:            constants.True,
				testAnotherAfterRebootAnnotation:     constants.True,
				constants.AnnotationRebootInProgress: constants.False,
			},
		},
	}
}

func node(ctx context.Context, t *testing.T, nodeClient corev1client.NodeInterface, name string) *corev1.Node {
	t.Helper()

	node, err := nodeClient.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Getting node %q: %v", name, err)
	}

	return node
}
