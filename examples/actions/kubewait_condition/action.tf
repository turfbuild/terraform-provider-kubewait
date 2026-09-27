resource "kubernetes_manifest" "migrate" {
  manifest = yamldecode(file("${path.module}/migrate-job.yaml"))
  lifecycle {
    action_trigger {
      events     = [after_create]
      actions    = [action.kubewait_condition.migrate_done]
      on_failure = taint
    }
  }
}

action "kubewait_condition" "migrate_done" {
  config {
    api_version        = "batch/v1"
    kind               = "Job"
    namespace          = "app"
    name               = "migrate"
    success_conditions = [{ type = "Complete", status = "True" }]
    failure_conditions = [{ type = "Failed", status = "True" }]
    absent             = "failure"
    timeout            = "15m"
    progress_fields    = ["status.active", "status.succeeded", "status.failed"]
  }
}
