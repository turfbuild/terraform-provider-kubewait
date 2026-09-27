action "kubewait_condition" "api_rolled_out" {
  config {
    api_version = "apps/v1"
    kind        = "Deployment"
    namespace   = "app"
    name        = "api"
    expression  = <<-EOT
      has(object.status.observedGeneration) &&
      object.status.observedGeneration >= object.metadata.generation &&
      has(object.status.updatedReplicas) && object.status.updatedReplicas == object.spec.replicas &&
      has(object.status.availableReplicas) && object.status.availableReplicas == object.spec.replicas &&
      object.status.replicas == object.spec.replicas
    EOT
    failure_conditions = [
      { type = "Progressing", status = "False", reason = "ProgressDeadlineExceeded" },
    ]
    timeout         = "15m"
    progress_fields = ["status.updatedReplicas", "status.availableReplicas", "status.conditions"]
  }
}
