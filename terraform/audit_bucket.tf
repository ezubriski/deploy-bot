# Optional managed S3 audit bucket with WORM compliance.
# Created when create_audit_bucket is true.

locals {
  audit_bucket_name        = var.create_audit_bucket ? aws_s3_bucket.audit[0].id : var.audit_bucket
  audit_bucket_enabled     = var.create_audit_bucket || var.audit_bucket != ""
  audit_bucket_cmk         = var.audit_bucket_kms_key_arn != ""
  create_access_log        = var.create_audit_bucket && var.create_audit_access_log_bucket
  access_log_enabled       = var.create_audit_bucket && (var.create_audit_access_log_bucket || var.audit_access_log_bucket != "")
  access_log_target_bucket = local.create_access_log ? aws_s3_bucket.audit_access_logs[0].id : var.audit_access_log_bucket
}

check "access_log_bucket_mutual_exclusion" {
  assert {
    condition     = !(var.create_audit_access_log_bucket && var.audit_access_log_bucket != "")
    error_message = "create_audit_access_log_bucket and audit_access_log_bucket are mutually exclusive. Set one or the other, not both."
  }
}

resource "aws_s3_bucket" "audit" {
  count = var.create_audit_bucket ? 1 : 0

  bucket              = "${var.name}-audit-${var.account_id}"
  object_lock_enabled = true
  tags                = var.tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "audit" {
  count  = var.create_audit_bucket ? 1 : 0
  bucket = aws_s3_bucket.audit[0].id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_object_lock_configuration" "audit" {
  count  = var.create_audit_bucket ? 1 : 0
  bucket = aws_s3_bucket.audit[0].id

  rule {
    default_retention {
      mode = "COMPLIANCE"
      days = var.audit_bucket_retention_days
    }
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "audit" {
  count  = var.create_audit_bucket ? 1 : 0
  bucket = aws_s3_bucket.audit[0].id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = local.audit_bucket_cmk ? "aws:kms" : "AES256"
      kms_master_key_id = local.audit_bucket_cmk ? var.audit_bucket_kms_key_arn : null
    }
    bucket_key_enabled = local.audit_bucket_cmk ? true : null
  }
}

resource "aws_s3_bucket_ownership_controls" "audit" {
  count  = var.create_audit_bucket ? 1 : 0
  bucket = aws_s3_bucket.audit[0].id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "audit" {
  count  = var.create_audit_bucket ? 1 : 0
  bucket = aws_s3_bucket.audit[0].id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Access logging — log who accessed the audit logs.
resource "aws_s3_bucket" "audit_access_logs" {
  count = local.create_access_log ? 1 : 0

  bucket = "${var.name}-audit-access-logs-${var.account_id}"
  tags   = var.tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "audit_access_logs" {
  count  = local.create_access_log ? 1 : 0
  bucket = aws_s3_bucket.audit_access_logs[0].id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "audit_access_logs" {
  count  = local.create_access_log ? 1 : 0
  bucket = aws_s3_bucket.audit_access_logs[0].id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "audit_access_logs" {
  count  = local.create_access_log ? 1 : 0
  bucket = aws_s3_bucket.audit_access_logs[0].id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "audit_access_logs" {
  count  = local.create_access_log ? 1 : 0
  bucket = aws_s3_bucket.audit_access_logs[0].id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "audit_access_logs" {
  count  = local.create_access_log ? 1 : 0
  bucket = aws_s3_bucket.audit_access_logs[0].id

  rule {
    id     = "expire-access-logs"
    status = "Enabled"
    filter {}

    expiration {
      days = var.audit_bucket_retention_days
    }
  }
}

resource "aws_s3_bucket_logging" "audit" {
  count  = local.access_log_enabled ? 1 : 0
  bucket = aws_s3_bucket.audit[0].id

  target_bucket = local.access_log_target_bucket
  target_prefix = "${aws_s3_bucket.audit[0].id}/"
}

# Lifecycle — transition audit objects to Glacier after 90 days.
resource "aws_s3_bucket_lifecycle_configuration" "audit" {
  count  = var.create_audit_bucket ? 1 : 0
  bucket = aws_s3_bucket.audit[0].id

  rule {
    id     = "archive-to-glacier"
    status = "Enabled"
    filter {}

    transition {
      days          = 90
      storage_class = "GLACIER"
    }
  }
}

data "aws_iam_policy_document" "audit_bucket" {
  count = var.create_audit_bucket ? 1 : 0

  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      aws_s3_bucket.audit[0].arn,
      "${aws_s3_bucket.audit[0].arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  statement {
    sid    = "DenyDeleteBucket"
    effect = "Deny"
    actions = [
      "s3:DeleteBucket",
      "s3:DeleteBucketPolicy",
    ]
    resources = [aws_s3_bucket.audit[0].arn]

    principals {
      type        = "*"
      identifiers = ["*"]
    }
  }
}

resource "aws_s3_bucket_policy" "audit" {
  count  = var.create_audit_bucket ? 1 : 0
  bucket = aws_s3_bucket.audit[0].id
  policy = data.aws_iam_policy_document.audit_bucket[0].json
}
