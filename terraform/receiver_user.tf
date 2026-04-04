# IAM user for the receiver component.
# Created when identity_type is "user". No trust policy needed.

resource "aws_iam_user" "receiver" {
  count = local.create_users ? 1 : 0

  name                 = "${var.name}-receiver"
  permissions_boundary = var.permissions_boundary != "" ? var.permissions_boundary : null
  tags                 = var.tags
}

resource "aws_iam_user_policy_attachment" "receiver" {
  count = local.create_users ? 1 : 0

  user       = aws_iam_user.receiver[0].name
  policy_arn = aws_iam_policy.receiver.arn
}
