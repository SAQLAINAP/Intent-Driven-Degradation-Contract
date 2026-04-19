// Package k8s contains the controller-runtime based Kubernetes reconciler.
// It manages replica counts, HPA bounds, rollout gating, and node scheduling
// for the target Deployment based on the active IDDC tier.
package k8s

import (
	"context"
	"fmt"
	"strconv"
	"sync"
)

// ReplicaAction describes what the controller should do to the Deployment replicas.
type ReplicaAction struct {
	// SetReplicas, if > 0, sets the Deployment replica count to exactly this value.
	SetReplicas int32
	// DisableHPA, if true, neutralises the HPA so the controller owns replica count directly.
	DisableHPA bool
	// BlockRollouts, if true, pauses the Deployment to prevent new rollouts.
	BlockRollouts bool
	// BlockScheduling, if true, cordons all cluster nodes (survival tier).
	BlockScheduling bool
}

// TierToReplicaAction returns the replica management action for a given tier.
func TierToReplicaAction(tier string, currentReplicas, targetWarm, maxPods int32) ReplicaAction {
	switch tier {
	case "nominal":
		return ReplicaAction{}
	case "warm":
		replicas := currentReplicas
		if replicas < targetWarm {
			replicas = targetWarm
		}
		return ReplicaAction{SetReplicas: replicas}
	case "hot":
		return ReplicaAction{SetReplicas: maxPods, DisableHPA: true}
	case "critical":
		return ReplicaAction{SetReplicas: currentReplicas, BlockRollouts: true}
	case "survival":
		return ReplicaAction{SetReplicas: currentReplicas, BlockRollouts: true, BlockScheduling: true}
	default:
		return ReplicaAction{}
	}
}

// Config holds parameters for the K8s reconciler.
type Config struct {
	KubeconfigPath string
	Namespace      string
	DeploymentName string
	TargetWarm     int32
	MaxPods        int32
}

// Reconciler manages a Kubernetes Deployment in response to IDDC tier transitions.
type Reconciler struct {
	cfg           Config
	client        *k8sClient
	mu            sync.Mutex
	cordonedNodes map[string]bool // nodes IDDC cordoned; tracked in-memory for uncordon
}

// NewReconciler creates a Reconciler. Returns an error if it cannot reach the cluster.
func NewReconciler(cfg Config) (*Reconciler, error) {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	c, err := newK8sClient(cfg.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("k8s: build client: %w", err)
	}
	return &Reconciler{
		cfg:           cfg,
		client:        c,
		cordonedNodes: map[string]bool{},
	}, nil
}

const (
	annHPAOriginalMin = "iddc.io/hpa-original-min"
	annHPAOriginalMax = "iddc.io/hpa-original-max"
)

// Reconcile applies tier-appropriate replica, rollout, and scheduling policy.
// It is safe to call from multiple goroutines.
func (r *Reconciler) Reconcile(ctx context.Context, tier string) error {
	deploy, err := r.client.GetDeployment(ctx, r.cfg.Namespace, r.cfg.DeploymentName)
	if err != nil {
		return fmt.Errorf("k8s: get deployment %s/%s: %w", r.cfg.Namespace, r.cfg.DeploymentName, err)
	}

	currentReplicas := deploy.Spec.Replicas
	if currentReplicas == 0 {
		currentReplicas = 1
	}

	action := TierToReplicaAction(tier, currentReplicas, r.cfg.TargetWarm, r.cfg.MaxPods)

	// 1. Replica count.
	if action.SetReplicas > 0 && currentReplicas != action.SetReplicas {
		if err := r.client.PatchDeploymentReplicas(ctx, r.cfg.Namespace, r.cfg.DeploymentName, action.SetReplicas); err != nil {
			return fmt.Errorf("k8s: patch replicas: %w", err)
		}
		fmt.Printf("[k8s] %s/%s: replicas %d → %d (tier=%s)\n",
			r.cfg.Namespace, r.cfg.DeploymentName, currentReplicas, action.SetReplicas, tier)
	}

	// 2. HPA management.
	if action.DisableHPA {
		if err := r.lockHPA(ctx, deploy, action.SetReplicas); err != nil {
			fmt.Printf("[k8s] HPA lock failed (non-fatal): %v\n", err)
		}
	} else if tier == "nominal" {
		if err := r.restoreHPA(ctx, deploy); err != nil {
			fmt.Printf("[k8s] HPA restore failed (non-fatal): %v\n", err)
		}
	}

	// 3. Pause/unpause Deployment to gate rollouts.
	if deploy.Spec.Paused != action.BlockRollouts {
		if err := r.client.PatchDeploymentPaused(ctx, r.cfg.Namespace, r.cfg.DeploymentName, action.BlockRollouts); err != nil {
			return fmt.Errorf("k8s: set paused=%v: %w", action.BlockRollouts, err)
		}
		fmt.Printf("[k8s] %s/%s: paused=%v (tier=%s)\n",
			r.cfg.Namespace, r.cfg.DeploymentName, action.BlockRollouts, tier)
	}

	// 4. Node scheduling (survival tier cordons all nodes to prevent new pod placement).
	if action.BlockScheduling {
		if err := r.cordonAll(ctx); err != nil {
			fmt.Printf("[k8s] node cordon failed (non-fatal): %v\n", err)
		}
	} else {
		r.uncordonOwned(ctx) // best-effort: uncordon nodes IDDC owns
	}

	return nil
}

// lockHPA saves the current HPA bounds to Deployment annotations, then sets min=max=replicas
// to prevent autoscaling from overriding the tier-mandated replica count.
func (r *Reconciler) lockHPA(ctx context.Context, deploy *Deployment, replicas int32) error {
	h, err := r.client.GetHPA(ctx, r.cfg.Namespace, r.cfg.DeploymentName)
	if err != nil {
		return fmt.Errorf("get HPA: %w", err)
	}

	// Store original bounds only on the first lock (idempotent).
	if deploy.Metadata.Annotations == nil || deploy.Metadata.Annotations[annHPAOriginalMin] == "" {
		origMin := int32(1)
		if h.Spec.MinReplicas != nil {
			origMin = *h.Spec.MinReplicas
		}
		_ = r.client.AnnotateDeployment(ctx, r.cfg.Namespace, r.cfg.DeploymentName,
			annHPAOriginalMin, strconv.Itoa(int(origMin)))
		_ = r.client.AnnotateDeployment(ctx, r.cfg.Namespace, r.cfg.DeploymentName,
			annHPAOriginalMax, strconv.Itoa(int(h.Spec.MaxReplicas)))
	}

	return r.client.PatchHPABounds(ctx, r.cfg.Namespace, r.cfg.DeploymentName, replicas, replicas)
}

// restoreHPA reads original HPA bounds from Deployment annotations and re-applies them.
func (r *Reconciler) restoreHPA(ctx context.Context, deploy *Deployment) error {
	ann := deploy.Metadata.Annotations
	if ann == nil {
		return nil
	}
	minStr, okMin := ann[annHPAOriginalMin]
	maxStr, okMax := ann[annHPAOriginalMax]
	if !okMin || !okMax || minStr == "" || maxStr == "" {
		return nil // HPA was never locked by IDDC
	}

	origMin, err1 := strconv.ParseInt(minStr, 10, 32)
	origMax, err2 := strconv.ParseInt(maxStr, 10, 32)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("parse HPA annotation: min=%q max=%q", minStr, maxStr)
	}

	if err := r.client.PatchHPABounds(ctx, r.cfg.Namespace, r.cfg.DeploymentName, int32(origMin), int32(origMax)); err != nil {
		return err
	}
	// Clear saved annotations.
	_ = r.client.AnnotateDeployment(ctx, r.cfg.Namespace, r.cfg.DeploymentName, annHPAOriginalMin, "")
	_ = r.client.AnnotateDeployment(ctx, r.cfg.Namespace, r.cfg.DeploymentName, annHPAOriginalMax, "")
	fmt.Printf("[k8s] HPA restored: min=%d max=%d\n", origMin, origMax)
	return nil
}

// cordonAll marks all cluster nodes as unschedulable to block new pod placement
// during survival tier. It tracks which nodes it cordons for later uncordon.
func (r *Reconciler) cordonAll(ctx context.Context) error {
	nodes, err := r.client.ListNodes(ctx)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var cordoned int
	for _, node := range nodes {
		if !node.Spec.Unschedulable {
			if err := r.client.SetNodeUnschedulable(ctx, node.Metadata.Name, true); err != nil {
				fmt.Printf("[k8s] cordon node %s failed: %v\n", node.Metadata.Name, err)
				continue
			}
			r.cordonedNodes[node.Metadata.Name] = true
			cordoned++
		}
	}
	fmt.Printf("[k8s] cordoned %d nodes (survival tier)\n", cordoned)
	return nil
}

// uncordonOwned uncordons only nodes that IDDC previously cordoned.
func (r *Reconciler) uncordonOwned(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for name := range r.cordonedNodes {
		if err := r.client.SetNodeUnschedulable(ctx, name, false); err != nil {
			fmt.Printf("[k8s] uncordon node %s failed: %v\n", name, err)
			continue
		}
		delete(r.cordonedNodes, name)
	}
}
