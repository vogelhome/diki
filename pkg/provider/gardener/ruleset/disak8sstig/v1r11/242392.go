// SPDX-FileCopyrightText: 2023 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package v1r11

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"k8s.io/component-base/version"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/diki/imagevector"
	"github.com/gardener/diki/pkg/kubernetes/pod"
	kubeutils "github.com/gardener/diki/pkg/kubernetes/utils"
	"github.com/gardener/diki/pkg/provider/gardener/internal/utils"
	"github.com/gardener/diki/pkg/rule"
	"github.com/gardener/diki/pkg/shared/images"
)

var _ rule.Rule = &Rule242392{}

type Rule242392 struct {
	InstanceID              string
	ControlPlaneClient      client.Client
	ControlPlaneNamespace   string
	ClusterClient           client.Client
	ClusterCoreV1RESTClient rest.Interface
	ClusterPodContext       pod.PodContext
	Logger                  *slog.Logger
}

func (r *Rule242392) ID() string {
	return ID242392
}

func (r *Rule242392) Name() string {
	return "The Kubernetes kubelet must enable explicit authorization (HIGH 242392)"
}

func (r *Rule242392) Run(ctx context.Context) (rule.RuleResult, error) {
	shootTarget := rule.NewTarget("cluster", "shoot")
	clusterNodes, err := kubeutils.GetNodes(ctx, r.ClusterClient, 300)
	if err != nil {
		return rule.SingleCheckResult(r, rule.ErroredCheckResult(err.Error(), shootTarget.With("kind", "nodeList"))), nil
	}

	clusterPods, err := kubeutils.GetPods(ctx, r.ClusterClient, "", labels.NewSelector(), 300)
	if err != nil {
		return rule.SingleCheckResult(r, rule.ErroredCheckResult(err.Error(), shootTarget.With("kind", "podList"))), nil
	}

	clusterWorkers, err := utils.GetWorkers(ctx, r.ControlPlaneClient, r.ControlPlaneNamespace, 300)
	if err != nil {
		return rule.SingleCheckResult(r, rule.ErroredCheckResult(err.Error(), rule.NewTarget("cluster", "seed", "kind", "workerList"))), nil
	}

	image, err := imagevector.ImageVector().FindImage(images.DikiOpsImageName)
	if err != nil {
		return rule.RuleResult{}, fmt.Errorf("failed to find image version for %s: %w", images.DikiOpsImageName, err)
	}
	image.WithOptionalTag(version.Get().GitVersion)

	checkResults := []rule.CheckResult{}
	nodesAllocatablePodsNum := kubeutils.GetNodesAllocatablePodsNum(clusterPods, clusterNodes)
	workerGroupNodes := utils.GetSingleAllocatableNodePerWorker(clusterWorkers, clusterNodes, nodesAllocatablePodsNum)

	// TODO use maps.Keys when released with go 1.21 or 1.22
	workerGroups := make([]string, 0, len(workerGroupNodes))
	for workerGroup := range workerGroupNodes {
		workerGroups = append(workerGroups, workerGroup)
	}
	slices.Sort(workerGroups)

	for _, workerGroup := range workerGroups {
		node, ok := workerGroupNodes[workerGroup]
		if !ok {
			// this should never happen
			checkResults = append(checkResults, rule.ErroredCheckResult(fmt.Sprintf("Failed retrieving node for worker group %s", workerGroup), rule.NewTarget()))
			continue
		}
		checkResult := r.checkWorkerGroup(ctx, workerGroup, node, image.String())
		checkResults = append(checkResults, checkResult)
	}

	const authorizationModeConfigOption = "authorization.mode"
	for _, clusterNode := range clusterNodes {
		target := shootTarget.With("kind", "node", "name", clusterNode.Name)
		if !kubeutils.NodeReadyStatus(clusterNode) {
			checkResults = append(checkResults, rule.WarningCheckResult("Node is not in Ready state.", target))
			continue
		}

		kubeletConfig, err := kubeutils.GetNodeConfigz(ctx, r.ClusterCoreV1RESTClient, clusterNode.Name)
		if err != nil {
			checkResults = append(checkResults, rule.ErroredCheckResult(err.Error(), target))
			continue
		}

		switch {
		case kubeletConfig.Authorization.Mode == nil:
			checkResults = append(checkResults, rule.FailedCheckResult(fmt.Sprintf("Option %s not set.", authorizationModeConfigOption), target))
		case *kubeletConfig.Authorization.Mode != "Webhook":
			checkResults = append(checkResults, rule.FailedCheckResult(fmt.Sprintf("Option %s set to not allowed value.", authorizationModeConfigOption), target.With("details", fmt.Sprintf("Authorization Mode set to %s", *kubeletConfig.Authorization.Mode))))
		default:
			checkResults = append(checkResults, rule.PassedCheckResult(fmt.Sprintf("Option %s set to allowed value.", authorizationModeConfigOption), target))
		}
	}

	return rule.RuleResult{
		RuleID:       r.ID(),
		RuleName:     r.Name(),
		CheckResults: checkResults,
	}, nil
}

func (r *Rule242392) checkWorkerGroup(ctx context.Context, workerGroup string, node utils.AllocatableNode, privPodImage string) rule.CheckResult {
	target := rule.NewTarget("cluster", "seed", "kind", "workerGroup", "name", workerGroup)
	if !node.Allocatable {
		return rule.WarningCheckResult("There are no ready nodes with at least 1 allocatable spot for worker group.", target)
	}

	podName := fmt.Sprintf("diki-%s-%s", IDNodeFiles, Generator.Generate(10))
	podTarget := rule.NewTarget("cluster", "shoot", "kind", "pod", "namespace", "kube-system", "name", podName)

	defer func() {
		if err := r.ClusterPodContext.Delete(ctx, podName, "kube-system"); err != nil {
			r.Logger.Error(err.Error())
		}
	}()

	additionalLabels := map[string]string{
		pod.LabelInstanceID: r.InstanceID,
	}
	clusterPodExecutor, err := r.ClusterPodContext.Create(ctx, pod.NewPrivilegedPod(podName, "kube-system", privPodImage, node.Node.Name, additionalLabels))
	if err != nil {
		return rule.ErroredCheckResult(err.Error(), podTarget)
	}

	commandResult, err := clusterPodExecutor.Execute(ctx, "sh", `curl -ksS https://127.0.0.1:10250/healthz`)
	if err != nil {
		return rule.ErroredCheckResult(err.Error(), podTarget)
	}

	if !strings.HasSuffix(strings.ToLower(strings.TrimSpace(commandResult)), "unauthorized") {
		return rule.FailedCheckResult("Kubelet always allows access (or could not be probed).", target)
	}

	rawKubeletCommand, err := kubeutils.GetKubeletCommand(ctx, clusterPodExecutor)
	if err != nil {
		return rule.ErroredCheckResult(err.Error(), podTarget)
	}

	const (
		authorizationModeConfigOption = "authorization.mode"
		authorizationModeFlag         = "authorization-mode"
	)

	if kubeutils.IsFlagSet(rawKubeletCommand, authorizationModeFlag) {
		return rule.FailedCheckResult(fmt.Sprintf("Use of deprecated kubelet config flag %s.", authorizationModeFlag), target)
	}

	kubeletConfig, err := kubeutils.GetKubeletConfig(ctx, clusterPodExecutor, rawKubeletCommand)
	if err != nil {
		return rule.ErroredCheckResult(err.Error(), podTarget)
	}

	if kubeletConfig.Authorization.Mode == nil {
		return rule.FailedCheckResult(fmt.Sprintf("Option %s not set.", authorizationModeConfigOption), target)
	}

	if *kubeletConfig.Authorization.Mode != "Webhook" {
		return rule.FailedCheckResult(fmt.Sprintf("Option %s set to not allowed value.", authorizationModeConfigOption), target.With("details", fmt.Sprintf("Authorization Mode set to %s", *kubeletConfig.Authorization.Mode)))
	}

	return rule.PassedCheckResult(fmt.Sprintf("Option %s set to allowed value.", authorizationModeConfigOption), target)
}
