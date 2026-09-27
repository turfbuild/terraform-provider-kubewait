terraform {
  required_version = ">= 1.16.0"
  required_providers {
    kubewait = {
      source  = "turfbuild/kubewait"
      version = "~> 0.1"
    }
  }
}

provider "kubewait" {
  host                   = module.eks_cluster.endpoint
  cluster_ca_certificate = base64decode(module.eks_cluster.certificate_authority_data)
  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", module.eks_cluster.cluster_name]
  }
}
