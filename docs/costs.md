# Costs and limits

You pay AWS directly for what truner uses in your account. truner itself has no license,
fee or subscription.

## What you pay for

| Item | Cost |
|---|---|
| **Control plane** | Effectively $0 while idle. Lambda, SQS, DynamoDB (on-demand), API Gateway and S3 cost cents to low dollars per month, even at 1,000 jobs per day. The scheduled janitor is the largest fixed item, at about $0.10 per month. |
| **Job machines** | The EC2 price for the seconds a machine runs (60 s minimum), plus its EBS volume. Spot is usually far below on-demand. |
| **Baked images** | AMI snapshots, roughly $0.50–$2 per image per month. truner keeps the newest two of each. |
| **Cache** | S3 storage for what you cache. Entries expire after 10 days by default (`--cache-days`). |
| **Warm pools** | Each ready machine's stopped EBS volume, for example 40 GiB gp3 ≈ $3.20 per month. |
| **NAT gateway** (optional) | About $32 per month plus $0.045 per GB, only with `--nat` for a static egress IP. |

Cache traffic goes through the VPC's free S3 gateway endpoint, so the cache adds no
data transfer charges inside the region.

## Seeing what you spend

```bash
truner costs                  # last 30 days by repository
truner costs --by workflow
truner costs --by type        # by instance type
truner jobs                   # per-job cost
truner status                 # today's jobs and spend, this month's estimate
```

Costs are estimates. truner computes them from each machine's actual spot or
on-demand price and runtime, and its disk. `truner costs` also shows what the same
minutes would have cost on GitHub-hosted runners of the same size, using GitHub's list
prices for larger runners.

For exact numbers, use AWS billing. Every machine carries the tags `truner:repository`,
`truner:workflow` and `truner:job-id`. Activate them as
[cost allocation tags](https://docs.aws.amazon.com/awsaccountbilling/latest/aboutv2/activating-tags.html)
in the Billing console to break down your bill by repository. Add your own tags to
profiles with `tags:` (see [configuration](configuration.md#runner-spec-keys)).

## Limits

Set limits in the account config (`truner config edit`):

```yaml
limits:
  maxInstances: 100        # concurrent machines (default 100)
  monthlyBudgetUsd: 500    # hard stop
```

- **`maxInstances`**: jobs wait in the queue while this many machines are running.
- **`monthlyBudgetUsd`**: once the month's estimated spend reaches the budget, new jobs
  wait in the queue and an alert is sent. Running jobs are not interrupted.

Each machine also has limits of its own:

| Label | Default | Meaning |
|---|---|---|
| `timeout` | 12 h | hard limit on the machine's lifetime |
| `idle-timeout` | 15 min | shut down if no job is picked up in this time |

## Alerts and reports

```bash
truner install --alert-email ops@example.com --daily-report
```

The address receives budget alerts, quota exhaustion alerts (at most hourly) and
dead-letter queue alarms. With `--daily-report` it also gets a daily summary of jobs and
spend. AWS sends a confirmation email for the subscription first, so click the link in it.

## Keeping costs down

- Keep the default `spot=true`. `spot=only` never falls back to on-demand.
- Ask for ranges (`cpu=4-8`) or leave out `family` so EC2 can choose from more instance
  types.
- Use `arm64` where your toolchain supports it. Graviton is usually cheaper for the same
  performance.
- Use the magic cache and Docker layer caching to shorten jobs. You pay per second.
- Keep warm pools small. Each ready machine costs its stopped disk.
