package optimizer

import (
	"context"
	"fmt"
	"log"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// CleanupUnusedResources returns cleanup recommendations and optionally deletes resources when dryRun=false.
// If namespace is empty, it scans all namespaces.
func CleanupUnusedResources(ctx context.Context, clientset *kubernetes.Clientset, namespace string, dryRun bool) ([]CleanupRecommendation, error) {
	recommendations := make([]CleanupRecommendation, 0)

	configMaps, err := clientset.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list configmaps: %w", err)
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	configMapsInUse := make(map[string]bool)
	for _, pod := range pods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.ConfigMap != nil {
				key := fmt.Sprintf("%s/%s", pod.Namespace, volume.ConfigMap.Name)
				configMapsInUse[key] = true
			}
		}

		for _, container := range pod.Spec.Containers {
			for _, env := range container.Env {
				if env.ValueFrom != nil && env.ValueFrom.ConfigMapKeyRef != nil {
					key := fmt.Sprintf("%s/%s", pod.Namespace, env.ValueFrom.ConfigMapKeyRef.Name)
					configMapsInUse[key] = true
				}
			}
		}
	}

	for _, cm := range configMaps.Items {
		key := fmt.Sprintf("%s/%s", cm.Namespace, cm.Name)
		if configMapsInUse[key] {
			continue
		}

		rec := CleanupRecommendation{
			ResourceType: "ConfigMap",
			Namespace:    cm.Namespace,
			Name:         cm.Name,
			Reason:       "Not referenced by any pod",
			AgeSeconds:   int64(time.Since(cm.CreationTimestamp.Time).Seconds()),
		}
		recommendations = append(recommendations, rec)

		if dryRun {
			continue
		}

		if err := clientset.CoreV1().ConfigMaps(cm.Namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{}); err != nil {
			log.Printf("Failed to delete configmap %s/%s: %v", cm.Namespace, cm.Name, err)
		} else {
			log.Printf("Deleted unused configmap %s/%s", cm.Namespace, cm.Name)
		}
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase != v1.PodFailed && pod.Status.Phase != v1.PodSucceeded {
			continue
		}

		age := time.Since(pod.CreationTimestamp.Time)
		if age <= 7*24*time.Hour {
			continue
		}

		rec := CleanupRecommendation{
			ResourceType: "Pod",
			Namespace:    pod.Namespace,
			Name:         pod.Name,
			Reason:       fmt.Sprintf("Failed/Completed pod older than 7 days (status: %s)", pod.Status.Phase),
			AgeSeconds:   int64(age.Seconds()),
		}
		recommendations = append(recommendations, rec)

		if dryRun {
			continue
		}

		if err := clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			log.Printf("Failed to delete pod %s/%s: %v", pod.Namespace, pod.Name, err)
		} else {
			log.Printf("Deleted old pod %s/%s", pod.Namespace, pod.Name)
		}
	}

	return recommendations, nil
}
