# Kubecon US 2019 Demo

1. Create a 1000 node **Kubemark** cluster
    ```
    cd $GOPATH/src/k8s.io/kubernetes
    #make quick-release
    source $GOPATH/src/github.com/mm4tt/k8s-util/set-common-envs/set-common-envs.sh preset-e2e-kubemark-common
    source $GOPATH/src/github.com/mm4tt/k8s-util/set-common-envs/set-common-envs.sh preset-e2e-kubemark-gce-scale
    
    kubetest \
      --provider=gce \
      --extract=gs://kubernetes-release/release/stable-1.18.txt \
      --gcp-project=$PROJECT \
      --gcp-zone=$ZONE \
      --gcp-node-size=n1-standard-8 \
      --gcp-nodes=15 \
      --kubemark \
      --kubemark-nodes=1000 \
      --up \
      --test-cmd=/bin/true
    ```
1. Get kubeconfigs for the two clusters
   ```
   export KUBEMARK_KUBECONFIG=$GOPATH/src/k8s.io/kubernetes/test/kubemark/resources/kubeconfig.kubemark
   export ROOT_KUBECONFIG=$HOME/.kube/config
   ```
   
1. Verify that everything looks good
   ```
   kubectl --kubeconfig=$ROOT_KUBECONFIG get nodes
   
   kubectl --kubeconfig=$KUBEMARK_KUBECONFIG get nodes | head
   ```
    
1. Run **ClusterLoader2**:
    ```
    $GOPATH/src/k8s.io/perf-tests/clusterloader2/run-e2e.sh \
      --provider=kubemark \
      --kubeconfig=$KUBEMARK_KUBECONFIG \
      --kubemark-root-kubeconfig=$ROOT_KUBECONFIG \
      --enable-prometheus-server \
      --tear-down-prometheus-server=false \
      --report-dir=/tmp/kubecon-us-2019/results \
      --testconfig=testing/kubecon-us-2019/config.yaml 2>&1 | tee /tmp/kubecon-us-2019/log
    ```
1. Set up proxy to grafana pod
   ```
   kubectl --kubeconfig $ROOT_KUBECONFIG -n monitoring port-forward svc/grafana 3000
   ```  
  
1. Let's see how the test goes - http://localhost:3000

1. Check the CL2 results

    