resource "terraform_data" "cluster_contents" {
  triggers_replace = module.eks_cluster.endpoint
  depends_on       = [module.eks_nodes]
  lifecycle {
    action_trigger {
      events  = [before_destroy]
      actions = [action.kubewait_condition.no_load_balancers]
    }
  }
}

resource "helm_release" "gateway" {
  # ...
  depends_on = [terraform_data.cluster_contents]
}

action "kubewait_condition" "no_load_balancers" {
  config {
    api_version     = "v1"
    kind            = "Service"
    filter          = "object.spec.type == 'LoadBalancer'"
    min_matching    = 0
    max_matching    = 0
    timeout         = "10m"
    progress_fields = ["metadata.namespace", "metadata.name"]
  }
}
