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

// resetNodeCondition updates status of a Kubernetes node this flannel process
// is running on to reset NetworkUnavailable condition
//
// On certain clouds, namely Google Compute Engine, nodes come up explicitly
// marked with NetworkUnavailable condition which renders them unschedulable
// and Kubernetes expects this condition to be removed either by RouteController
// or networking plugins.
//
// Since flannel is managing routes (including creating routes using cloud APIs
// using proper backend) and thus it has to reset this condition itself.
//
// According to the current Kubernetes source code and multiple Github issues
// this is an "expected" way to workaround this and what other people are
// doing as well (https://github.com/kubernetes/kubernetes/issues/44254,
// https://github.com/weaveworks/weave/issues/3249).
func resetNodeCondition(ctx context.Context) {
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	// retry until we succeed because the node may not be registered yet
	// (kubelet comes up after flannel)
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
			log.Warning("Context is canceled, node status wasn't updated")
			return
		}
	}
}

// getKubeClient creates a new Kubernetes client using connection information
// provided via command-line flags
func getKubeClient() (*kubernetes.Clientset, error) {
	return kubernetes.NewForConfig(&rest.Config{
		Host: fmt.Sprintf("https://%v", opts.kubeAPIServer),
		TLSClientConfig: rest.TLSClientConfig{
			CertFile: opts.kubeCert,
			KeyFile:  opts.kubeKey,
			CAFile:   opts.kubeCA,
		},
	})
}

// formatPatch creates a patch that modifies a node status to remove
// NetworkUnavailable condition
func formatPatch() ([]byte, error) {
	bytes, err := json.Marshal(&[]v1.NodeCondition{{
		Type:               v1.NodeNetworkUnavailable,
		Status:             v1.ConditionFalse,
		Reason:             "RouteCreated",
		Message:            "Flannel created a route",
		LastTransitionTime: metav1.NewTime(time.Now()),
	}})
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`{"status":{"conditions":%s}}`, bytes)), nil
}

const (
	// retryInterval is how soon flannel will reattempt to update Kubernetes
	// node status in case of failure
	retryInterval = 2 * time.Second
)
