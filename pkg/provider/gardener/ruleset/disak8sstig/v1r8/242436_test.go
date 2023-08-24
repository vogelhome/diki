// SPDX-FileCopyrightText: 2023 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package v1r8_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	gomegatypes "github.com/onsi/gomega/types"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gardener/diki/pkg/provider/gardener"
	"github.com/gardener/diki/pkg/provider/gardener/ruleset/disak8sstig/v1r8"
	dikirule "github.com/gardener/diki/pkg/rule"
)

var _ = Describe("#242436", func() {
	var (
		fakeClient client.Client
		ctx        = context.TODO()
		namespace  = "foo"

		kcmDeployment *appsv1.Deployment
		target        = gardener.NewTarget("cluster", "seed", "name", "kube-apiserver", "namespace", namespace, "kind", "deployment")
	)

	BeforeEach(func() {
		fakeClient = fakeclient.NewClientBuilder().Build()
		kcmDeployment = &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kube-apiserver",
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:    "kube-apiserver",
								Command: []string{},
								Args:    []string{},
							},
						},
					},
				},
			},
		}
	})

	It("should error when kube-apiserver is not found", func() {
		rule := v1r8.Rule242436{Logger: testLogger, Client: fakeClient, Namespace: namespace}
		ruleResult, err := rule.Run(ctx)
		Expect(err).ToNot(HaveOccurred())

		Expect(ruleResult.CheckResults).To(Equal([]dikirule.CheckResult{
			{
				Status:  dikirule.Errored,
				Message: "deployments.apps \"kube-apiserver\" not found",
				Target:  target,
			},
		},
		))
	})

	DescribeTable("Run cases",
		func(container corev1.Container, expectedCheckResults []dikirule.CheckResult, errorMatcher gomegatypes.GomegaMatcher) {
			kcmDeployment.Spec.Template.Spec.Containers = []corev1.Container{container}
			Expect(fakeClient.Create(ctx, kcmDeployment)).To(Succeed())

			rule := v1r8.Rule242436{Logger: testLogger, Client: fakeClient, Namespace: namespace}
			ruleResult, err := rule.Run(ctx)
			Expect(err).To(errorMatcher)

			Expect(ruleResult.CheckResults).To(Equal(expectedCheckResults))
		},

		Entry("should pass when enable-admission-plugins is not set",
			corev1.Container{Name: "kube-apiserver", Command: []string{"--flag1=value1", "--flag2=value2"}},
			[]dikirule.CheckResult{{Status: dikirule.Passed, Message: "Option enable-admission-plugins has not been set.", Target: target}},
			BeNil()),
		Entry("should fail when enable-admission-plugins is set to not allowed values",
			corev1.Container{Name: "kube-apiserver", Command: []string{"--enable-admission-plugins=PodNodeSelector,PodSecurity"}},
			[]dikirule.CheckResult{{Status: dikirule.Failed, Message: "Option enable-admission-plugins set to not allowed value.", Target: target}},
			BeNil()),
		Entry("should pass when enable-admission-plugins is set to allowed value ValidatingAdmissionWebhook",
			corev1.Container{Name: "kube-apiserver", Command: []string{"--enable-admission-plugins=PodNodeSelector,ValidatingAdmissionWebhook,PodSecurity"}},
			[]dikirule.CheckResult{{Status: dikirule.Passed, Message: "Option enable-admission-plugins set to allowed value.", Target: target}},
			BeNil()),
		Entry("should warn when enable-admission-plugins is set more than once",
			corev1.Container{Name: "kube-apiserver", Command: []string{"--enable-admission-plugins=PodNodeSelector,PodSecurity"}, Args: []string{"--enable-admission-plugins=ValidatingAdmissionWebhook,PodNodeSelector"}},
			[]dikirule.CheckResult{{Status: dikirule.Warning, Message: "Option enable-admission-plugins has been set more than once in container command.", Target: target}},
			BeNil()),
		Entry("should error when deployment does not have container 'kube-apiserver'",
			corev1.Container{Name: "not-kube-apiserver", Command: []string{"--enable-admission-plugins=PodNodeSelector,PodSecurity"}},
			[]dikirule.CheckResult{{Status: dikirule.Errored, Message: "deployment: kube-apiserver does not contain container: kube-apiserver", Target: target}},
			BeNil()),
	)
})