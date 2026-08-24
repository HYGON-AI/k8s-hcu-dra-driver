// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
)

// resolveClaimConsumer finds the Pod/container using a ResourceClaim (for /etc/vdev/dynamic
// mark files consumed by hcu-exporter, same layout as k8s-hcu-device-plugin HAMI mode).
func resolveClaimConsumer(ctx context.Context, cs kubernetes.Interface, nodeName string, claim *resourceapi.ResourceClaim) (podUID, containerName string, err error) {
	if cs == nil {
		return "", "", fmt.Errorf("kubernetes client not configured")
	}

	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource != "pods" {
			continue
		}
		// ResourceClaimConsumerReference has no Namespace; consumer Pod is in the claim's namespace.
		pod, getErr := cs.CoreV1().Pods(claim.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if getErr != nil {
			continue
		}
		if nodeName != "" && pod.Spec.NodeName != "" && pod.Spec.NodeName != nodeName {
			continue
		}
		if c, ok := containerUsingResourceClaim(pod, claim.Name); ok {
			return string(pod.UID), c, nil
		}
		if len(pod.Spec.Containers) > 0 {
			return string(pod.UID), pod.Spec.Containers[0].Name, nil
		}
	}

	if nodeName != "" {
		selector := fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
		pods, listErr := cs.CoreV1().Pods(claim.Namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
		if listErr == nil {
			for i := range pods.Items {
				pod := &pods.Items[i]
				if c, ok := containerUsingResourceClaim(pod, claim.Name); ok {
					return string(pod.UID), c, nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("no pod consumer found for claim %s/%s", claim.Namespace, claim.Name)
}

func containerUsingResourceClaim(pod *corev1.Pod, claimName string) (string, bool) {
	claimRefName := ""
	for _, rc := range pod.Spec.ResourceClaims {
		if rc.ResourceClaimName != nil && *rc.ResourceClaimName == claimName {
			claimRefName = rc.Name
			break
		}
	}
	if claimRefName == "" {
		return "", false
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		for _, cr := range c.Resources.Claims {
			if cr.Name == claimRefName {
				return c.Name, true
			}
		}
	}
	return "", false
}
