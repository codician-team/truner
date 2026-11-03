# Networking and access

## Default: a dedicated VPC

`truner install` creates a VPC (`10.100.0.0/16`) with one public subnet in each of up to
three availability zones, an internet gateway and a free S3 gateway endpoint. Machines
get public IPs for outbound traffic. The security group allows **no inbound**
traffic.

## Static egress IP

To allowlist your runners somewhere (a database, a firewall, a SaaS API):

```bash
truner install --nat
```

This adds private subnets behind a NAT gateway with an Elastic IP (`StaticEgressIp`
in `truner status` and in the stack outputs). Jobs opt in with `private=true`, or for
all jobs set it in the account config:

```yaml
defaults:
  private: true
```

A NAT gateway costs about $32 per month plus $0.045/GB processed. S3 traffic, including
the cache, bypasses it through the gateway endpoint.

## Using your own VPC

```bash
truner install --network existing \
  --vpc-id vpc-0abc \
  --public-subnets subnet-1,subnet-2,subnet-3 \
  --private-subnets subnet-4,subnet-5      # optional, for private=true
```

"Public" subnets must give machines internet access: auto-assign public IPs, or a
route to a NAT. Add an S3 gateway endpoint to keep cache traffic off the internet.

## Debug access

### SSM Session Manager (always available)

Machines have the SSM agent and the `AmazonSSMManagedInstanceCore` policy. Open a shell
on a job's machine:

```bash
truner connect 123456789         # job id or instance id
```

This uses `aws ssm start-session`. It requires the AWS CLI and the Session Manager
plugin, and needs no open ports.

To inspect a machine after its job, add `keep`:

```yaml
runs-on: truner/cpu=4/keep=30m/run=${{ github.run_id }}
```

The job log banner shows how to release the machine early (`touch /var/lib/truner/run/done`).

### SSH with your GitHub keys

```bash
truner install --ssh-cidr 203.0.113.7/32
```

```yaml
runs-on: truner/cpu=4/ssh=true/keep=30m
```

The public SSH keys of the user who triggered the run (from `github.com/<user>.keys`)
are installed for the `runner` user. The job log shows the `ssh` command. Use
`truner connect --ssh <job>`.

## IAM: letting jobs use AWS

Machines run with an instance role that can only read and write the cache and push to
the ECR cache repository. To deploy from CI without stored keys, attach policies:

```bash
truner install --runner-policy arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

Every job of the installation gets these permissions. For production deployments
prefer GitHub OIDC (`aws-actions/configure-aws-credentials` with `role-to-assume`).
It works on truner like anywhere else and scopes access per repository and branch.
