# Merchant Payments Platform - Core Infrastructure
#
# This module provisions the foundational AWS infrastructure for the
# payment processing platform. It is designed for multi-AZ deployment
# with strict network isolation between the cardholder data environment
# (CDE) and general processing.
#
# PCI-DSS Note: Network segmentation is enforced at the VPC/subnet level.
# The CDE subnet has no internet access and is only reachable from the
# processing subnet via specific security group rules.

terraform {
  required_version = ">= 1.7.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.40"
    }
  }

  backend "s3" {
    bucket         = "payments-terraform-state"
    key            = "infrastructure/core/terraform.tfstate"
    region         = "me-south-1"
    dynamodb_table = "terraform-locks"
    encrypt        = true
  }
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project     = "merchant-payments-platform"
      Environment = var.environment
      ManagedBy   = "terraform"
      CostCenter  = "payments-engineering"
    }
  }
}

# ─── Variables ───

variable "aws_region" {
  type    = string
  default = "me-south-1"
}

variable "environment" {
  type    = string
  default = "production"
}

variable "vpc_cidr" {
  type    = string
  default = "10.0.0.0/16"
}

# ─── VPC and Networking ───

module "vpc" {
  source = "./modules/vpc"

  vpc_cidr    = var.vpc_cidr
  environment = var.environment
  azs         = ["${var.aws_region}a", "${var.aws_region}b", "${var.aws_region}c"]

  # Subnet layout:
  # Public:     10.0.0.0/20   (NLB, NAT Gateway)
  # Processing: 10.0.16.0/20  (EKS nodes, application services)
  # CDE:        10.0.32.0/20  (Databases, Redis, Kafka)
  # Management: 10.0.48.0/20  (Bastion, monitoring)
  public_subnets     = ["10.0.0.0/22", "10.0.4.0/22", "10.0.8.0/22"]
  processing_subnets = ["10.0.16.0/22", "10.0.20.0/22", "10.0.24.0/22"]
  cde_subnets        = ["10.0.32.0/22", "10.0.36.0/22", "10.0.40.0/22"]
  management_subnets = ["10.0.48.0/22", "10.0.52.0/22", "10.0.56.0/22"]
}

# ─── EKS Cluster ───

module "eks" {
  source = "./modules/eks"

  cluster_name    = "payments-${var.environment}"
  cluster_version = "1.29"
  vpc_id          = module.vpc.vpc_id
  subnet_ids      = module.vpc.processing_subnet_ids

  node_groups = {
    # Critical path services: payment gateway, auth engine
    critical = {
      instance_types = ["c6i.2xlarge"]
      min_size       = 6
      max_size       = 24
      desired_size   = 6
      labels = {
        tier = "critical-path"
      }
      taints = []
    }

    # ML inference: fraud engine with GPU
    ml_inference = {
      instance_types = ["g5.xlarge"]
      min_size       = 4
      max_size       = 16
      desired_size   = 8
      labels = {
        tier = "ml-inference"
      }
      taints = [{
        key    = "nvidia.com/gpu"
        value  = "true"
        effect = "NO_SCHEDULE"
      }]
    }

    # General workloads: settlement, notification, monitoring
    general = {
      instance_types = ["m6i.xlarge"]
      min_size       = 3
      max_size       = 12
      desired_size   = 4
      labels = {
        tier = "general"
      }
      taints = []
    }
  }

  # Enable IRSA for pod-level IAM
  enable_irsa = true
}

# ─── RDS PostgreSQL ───

module "rds_payments" {
  source = "./modules/rds"

  identifier     = "payments-${var.environment}"
  engine_version = "16.2"
  instance_class = "db.r6g.2xlarge"

  database_name  = "payments"
  port           = 5432
  multi_az       = true

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.cde_subnet_ids

  # Only allow connections from processing subnet
  allowed_security_groups = [module.eks.node_security_group_id]

  storage_type      = "io2"
  allocated_storage = 500
  iops              = 10000
  storage_encrypted = true

  backup_retention_period = 35
  deletion_protection     = true

  performance_insights_enabled = true
  monitoring_interval          = 15

  parameters = {
    "max_connections"                    = "400"
    "shared_buffers"                     = "{DBInstanceClassMemory/4}"
    "default_transaction_isolation"      = "serializable"
    "log_min_duration_statement"         = "100"
    "pg_stat_statements.track"           = "all"
    "auto_explain.log_min_duration"      = "200"
  }
}

module "rds_ledger" {
  source = "./modules/rds"

  identifier     = "ledger-${var.environment}"
  engine_version = "16.2"
  instance_class = "db.r6g.4xlarge"

  database_name  = "ledger"
  port           = 5432
  multi_az       = true

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.cde_subnet_ids

  allowed_security_groups = [module.eks.node_security_group_id]

  storage_type      = "io2"
  allocated_storage = 1000
  iops              = 20000
  storage_encrypted = true

  # Ledger database has stricter backup: 90 days
  backup_retention_period = 90
  deletion_protection     = true

  # Two read replicas for balance queries and reporting
  read_replica_count = 2

  parameters = {
    "max_connections"                    = "600"
    "shared_buffers"                     = "{DBInstanceClassMemory/4}"
    "default_transaction_isolation"      = "serializable"
    "synchronous_commit"                 = "on"
    "checkpoint_timeout"                 = "600"
    "wal_level"                          = "logical"
  }
}

module "rds_audit" {
  source = "./modules/rds"

  identifier     = "audit-${var.environment}"
  engine_version = "16.2"
  instance_class = "db.r6g.xlarge"

  database_name  = "audit"
  port           = 5432
  multi_az       = true

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.cde_subnet_ids

  allowed_security_groups = [module.eks.node_security_group_id]

  storage_type      = "io2"
  allocated_storage = 2000
  iops              = 5000
  storage_encrypted = true

  backup_retention_period = 365  # 1 year retention for audit
  deletion_protection     = true

  parameters = {
    "default_transaction_isolation" = "read committed"
    "max_connections"               = "100"
  }
}

# ─── ElastiCache Redis ───

module "redis" {
  source = "./modules/elasticache"

  cluster_id         = "payments-${var.environment}"
  engine_version     = "7.1"
  node_type          = "cache.r6g.xlarge"
  num_cache_clusters = 6  # 3 primary + 3 replica across AZs

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.cde_subnet_ids

  allowed_security_groups = [module.eks.node_security_group_id]

  at_rest_encryption_enabled = true
  transit_encryption_enabled = true

  snapshot_retention_limit = 7
}

# ─── MSK (Kafka) ───

module "msk" {
  source = "./modules/msk"

  cluster_name   = "payments-${var.environment}"
  kafka_version  = "3.6.0"
  instance_type  = "kafka.m5.2xlarge"
  number_of_brokers = 6  # 2 per AZ

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.cde_subnet_ids

  allowed_security_groups = [module.eks.node_security_group_id]

  ebs_volume_size = 1000
  encryption_in_transit = "TLS"

  # Schema Registry compatibility
  configuration = {
    "auto.create.topics.enable"  = "false"
    "default.replication.factor" = "3"
    "min.insync.replicas"        = "2"
    "message.max.bytes"          = "1048576"
    "log.retention.hours"        = "168"
  }
}

# ─── S3: Event archive and ML feature store ───

resource "aws_s3_bucket" "event_archive" {
  bucket = "payments-events-${var.environment}-${var.aws_region}"
}

resource "aws_s3_bucket_versioning" "event_archive" {
  bucket = aws_s3_bucket.event_archive.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "event_archive" {
  bucket = aws_s3_bucket.event_archive.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.payments.arn
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "event_archive" {
  bucket = aws_s3_bucket.event_archive.id

  rule {
    id     = "archive-old-events"
    status = "Enabled"

    transition {
      days          = 90
      storage_class = "GLACIER"
    }

    # Regulatory requirement: 7-year retention for financial transactions
    expiration {
      days = 2555
    }
  }
}

# ─── KMS: Encryption key for PII and sensitive data ───

resource "aws_kms_key" "payments" {
  description             = "Encryption key for payments platform PII and sensitive data"
  deletion_window_in_days = 30
  enable_key_rotation     = true

  policy = data.aws_iam_policy_document.kms_policy.json
}

resource "aws_kms_alias" "payments" {
  name          = "alias/payments-${var.environment}"
  target_key_id = aws_kms_key.payments.key_id
}

# ─── Outputs ───

output "eks_cluster_endpoint" {
  value = module.eks.cluster_endpoint
}

output "rds_payments_endpoint" {
  value     = module.rds_payments.endpoint
  sensitive = true
}

output "rds_ledger_endpoint" {
  value     = module.rds_ledger.endpoint
  sensitive = true
}

output "redis_endpoint" {
  value     = module.redis.primary_endpoint
  sensitive = true
}

output "msk_bootstrap_brokers" {
  value     = module.msk.bootstrap_brokers_tls
  sensitive = true
}
