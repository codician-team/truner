# Costs and limits

You pay AWS directly for what truner uses in your account. truner itself has no license,
fee or subscription.

## What you pay for

| Item | Cost |
|---|---|
| **Control plane** | Effectively $0 while idle. Lambda, SQS, DynamoDB (on-demand), API Gateway and S3 cost cents to low dollars per month, even at 1,000 jobs per day. The scheduled janitor is the largest fixed item, at about $0.10 per month. |
| **Job machines** | The EC2 price for the seconds a machine runs (60 s minimum), plus its EBS volume and, in a public subnet, $0.005 per hour for its public IPv4 address. Spot is usually far below on-demand. |
| **Baked images** | AMI snapshots, roughly $0.50–$2 per image per month. truner keeps the newest two of each. |
| **Cache** | S3 storage for what you cache, including [repository snapshots](caching.md#repository-snapshots). Entries expire after 10 days by default (`--cache-days`). |
| **Warm pools** | Each ready machine's stopped EBS volume, for example 40 GiB gp3 ≈ $3.20 per month. |
| **MicroVMs** (micro jobs) | Lambda's per-second price, about $0.126 per vCPU-hour including its memory, more while a job bursts, plus $0.00155 per GiB of snapshot read at each start. More than EC2 per job; see [MicroVMs](microvms.md#costs). |
| **NAT gateway** (optional) | About $32 per month plus $0.045 per GB, only with `--nat` for a static egress IP. |

Cache traffic goes through the VPC's free S3 gateway endpoint, so the cache adds no
data transfer charges inside the region.

## Seeing what you spend

```bash
truner costs                  # last 30 days by repository
truner costs --by workflow
truner costs --by type        # by instance type
truner jobs                   # per-job cost
truner status                 # today's jobs and spend, this month's spend
```

### Estimates first, then the bill

A job's cost starts as an estimate, and AWS's number replaces it once AWS has posted the
day, about a day after the day ended. `truner jobs` marks estimates with `~`:

```text
  Billed by AWS     $3.93 through 2026-11-05
  Estimated         $4.98 AWS has not billed these yet, or cannot show them as truner's
  Total             $8.91
```

**Estimates** use each machine's spot or on-demand price at launch in its availability
zone, its runtime, its disk with any provisioned IOPS and throughput, and its public IP.
For MicroVMs they use a burst factor and snapshot reads measured on real bills. Compared
with the bill for 2026-11-03 to 2026-11-05, EC2 estimates were within 1% in total, though
a single instance type could be off by up to about 50% before estimates used the right
zone's price. MicroVM estimates were within 3%, on 304 jobs of a few repositories, so yours
may differ until the bill arrives.

**The bill** comes from AWS Cost Explorer, which reports per day and machine type, not per
job. truner splits each amount between that day's jobs by their estimates:

- **EC2** counts only with the `truner:stack` tag, so other EC2 in your account stays out,
  even of the same instance type. Until that tag is active, EC2 jobs keep their estimate.
- **MicroVMs** cannot be tagged, so their amount is split between the micro jobs of all
  truner stacks in the region. If a MicroVM image that is not truner's exists in the
  region, MicroVM jobs keep their estimate.
- **The stack itself** (cache storage, images, the control plane) is shown on its own line
  once the tag is active.

Activate the tags once per AWS account. In an AWS Organization, this has to be done from
the management account:

```bash
truner costs --activate-tags
```

This activates `truner:stack`, `truner:repository` and `truner:workflow` as
[cost allocation tags](https://docs.aws.amazon.com/awsaccountbilling/latest/aboutv2/activating-tags.html),
and asks AWS to apply them to the bills since last month. AWS needs up to 24 hours. You
can then also break down your bill by repository in the Billing console. Add your own tags
to profiles with `tags:` (see [configuration](configuration.md#runner-spec-keys)).

Cost Explorer charges $0.01 per request. `truner costs` and `truner status` read the bill
at most every 12 hours, only while a day is not final, and keep the result in the stack's
table. `truner jobs` and `truner doctor` only read what is kept. `truner costs
--estimates-only` does not read the bill at all. Reading it needs `ce:GetCostAndUsage`.

Tax, credits and refunds are not included. Neither is anything in other regions, even
machines that a tool of yours launches with the same AWS user.

### Compared with GitHub-hosted runners

`truner costs` also shows what the same minutes would have cost on GitHub-hosted runners,
at [GitHub's list prices](https://docs.github.com/en/billing/reference/actions-runner-pricing)
for the smallest runner with as many cores as the job's machine, and for a MicroVM its
peak size. Free minutes included in your GitHub plan are not taken off.

## Limits

Set limits in the account config (`truner config edit`):

```yaml
limits:
  maxInstances: 100        # concurrent machines (default 100)
  monthlyBudgetUsd: 500    # hard stop
```

- **`maxInstances`**: jobs wait in the queue while this many machines are running.
  MicroVMs count as machines.
- **`monthlyBudgetUsd`**: an alert is sent at 80% of the budget. Once the month's
  estimated spend reaches it, another alert is sent and new jobs wait in the queue until
  you raise the budget or the month ends (UTC). Running jobs are not interrupted. The
  budget always uses the estimates and counts only job machines, not the bill.

Jobs that wait for a limit retry on their own, every 30 seconds for `maxInstances` and
every 10 minutes for the budget.

Each machine also has limits of its own:

| Label | Default | Meaning |
|---|---|---|
| `timeout` | 12 h | hard limit on the machine's lifetime (MicroVMs: at most 8 h) |
| `idle-timeout` | 15 min | shut down if no job is picked up in this time |

## Alerts and reports

```bash
truner install --alert-email ops@example.com --daily-report
```

The address receives budget alerts (at 80% and 100%), quota exhaustion alerts (at most
hourly), an alert for each job that could not get a machine after all retries, failed
image builds and dead-letter queue alarms. With `--daily-report` it also gets a summary
of the previous day's jobs and spend after 01:00 UTC, on days with jobs. AWS sends a
confirmation email for the subscription first, so click the link in it.

## Keeping costs down

- Keep the default `spot=true`. `spot=only` never falls back to on-demand.
- Ask for ranges (`cpu=4-8`) or leave out `family` so EC2 can choose from more instance
  types.
- Use `arm64` where your toolchain supports it. Graviton is usually cheaper for the same
  performance.
- Use the magic cache and Docker layer caching to shorten jobs. You pay per second.
- Keep warm pools small. Each ready machine costs its stopped disk.
