package main

import (
	"encoding/json"
	"fmt"
	"time"

	log "github.com/golang/glog"
	"golang.org/x/net/context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
)

func resetNodeCondition(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			client, err := getKubeClient()
			if err != nil {
				log.Warningf("Failed to create kube client: %v", err)
				continue
			}
			patch, err := formatPatch()
			if err != nil {
				log.Warningf("Failed to format patch: %v", err)
				continue
			}
			_, err = client.CoreV1().Nodes().PatchStatus(opts.kubeNode, patch)
			if err != nil {
				log.Warningf("Failed to update node status, will retry: %v", err)
				continue
			}
			log.Infof("Successfully updated node %q status", opts.kubeNode)
			return
		case <-ctx.Done():
			log.Warning("Context is canceled")
			return
		}
	}
}

func getKubeClient() (*kubernetes.Clientset, error) {
	return kubernetes.NewForConfig(&rest.Config{
		Host: fmt.Sprintf("https://%v:6443", opts.kubeAPIServer),
		TLSClientConfig: rest.TLSClientConfig{
			CertFile: opts.kubeCert,
			KeyFile:  opts.kubeKey,
			CAFile:   opts.kubeCA,
		},
	})
}

func formatPatch() ([]byte, error) {
	condition := v1.NodeCondition{
		Type:               v1.NodeNetworkUnavailable,
		Status:             v1.ConditionFalse,
		Reason:             "RouteCreated",
		Message:            "Flannel created a route",
		LastTransitionTime: metav1.NewTime(time.Now()),
	}
	bytes, err := json.Marshal(&[]v1.NodeCondition{condition})
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`{"status":{"conditions":%s}}`, bytes)), nil
}
