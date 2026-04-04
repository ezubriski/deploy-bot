# IAM user for the bot (worker) component.
# Created when identity_type is "user". No trust policy needed.

resource "aws_iam_user" "bot" {
  count = local.create_users ? 1 : 0

  name                 = "${var.name}-bot"
  permissions_boundary = var.permissions_boundary != "" ? var.permissions_boundary : null
  tags                 = var.tags
}

resource "aws_iam_user_policy_attachment" "bot" {
  count = local.create_users ? 1 : 0

  user       = aws_iam_user.bot[0].name
  policy_arn = aws_iam_policy.bot.arn
}
