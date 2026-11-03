# Caching

## The magic cache (default)

Every truner machine runs a small cache server. It speaks the GitHub Actions cache
protocols (the current twirp service used by `actions/cache` v4+, the Azure Blob
upload API it relies on, and the legacy REST API of `actions/cache` v3) and stores
entries in the installation's S3 bucket under `cache/<owner>/<repo>/`.

The runner is pointed at it transparently. Nothing changes in your workflows:

```yaml
- uses: actions/cache@v6
  with:
    path: ~/.cache/go-build
    key: go-${{ hashFiles('**/go.sum') }}
    restore-keys: go-
- uses: actions/setup-node@v7
  with: { node-version: 22, cache: pnpm }
```

**How it works.** The GitHub runner always overwrites the cache service URL it
hands to actions. truner patches the runner on each machine, renaming that internal
assignment, and sets `ACTIONS_RESULTS_URL`/`ACTIONS_CACHE_URL` to the local proxy for
host steps, container steps and container actions. Artifact uploads and other
non-cache calls are forwarded to GitHub untouched. If the runner ever changes in a way
the patch does not recognize, the machine falls back to GitHub's cache service and
the job log says so.

**Semantics** follow GitHub's:
- `key` must match exactly; `restore-keys` match by prefix, newest entry first.
- Entries are isolated by cache *version* (paths + compression), so caches from
  different paths or operating systems never collide.
- Branch scoping: a run reads caches of its own ref first, then of the base/default
  branch. It only writes to its own ref. Pull requests cannot poison `main`.
- An existing key is never overwritten.

**Limits and retention.** No size limit. Entries expire after 10 days by default
(`truner install --cache-days 30` to change it). Manage entries with
`truner cache ls|stats|rm|purge`.

**Speed.** Traffic stays in-region and goes through the VPC's free S3 gateway endpoint.
Downloads use parallel ranged reads (8 × 16 MiB in flight) and typically reach
hundreds of MB/s on mid-size instances: a 234 MB npm cache restores in about a second
(289–337 MiB/s on 4- and 8-vCPU EC2 machines and 4-vCPU MicroVMs).

**What it does not cover.** `actions/checkout` fetches the repository from GitHub on
every machine, past the cache. For a 237 MB repository that took 11–102 seconds on EC2
and about 17 on MicroVMs. See
[Splitting CI into parallel jobs](parallel-jobs.md#what-a-job-costs-before-its-first-step).

Opt out per job with `cache=github` (GitHub's service) or `cache=off`.

## Docker layer caching

### `type=gha` (zero configuration)

`docker buildx` with the GitHub Actions cache backend goes through the magic cache too:

```yaml
- uses: docker/setup-buildx-action@v3
- uses: docker/build-push-action@v6
  with:
    cache-from: type=gha
    cache-to: type=gha,mode=max
```

### Registry cache in ECR

Each installation has an ECR repository for build caches. Machines are already logged
in, and `$TRUNER_ECR_CACHE` holds its URI:

```yaml
- uses: docker/build-push-action@v6
  with:
    cache-from: type=registry,ref=${{ env.TRUNER_ECR_CACHE }}:myapp
    cache-to: type=registry,ref=${{ env.TRUNER_ECR_CACHE }}:myapp,mode=max,image-manifest=true,oci-mediatypes=true
```

Images in that repository expire after 14 days.

### S3 cache backend

```yaml
- uses: docker/build-push-action@v6
  with:
    cache-from: type=s3,region=${{ env.AWS_REGION }},bucket=${{ env.TRUNER_S3_BUCKET }},prefix=${{ env.TRUNER_S3_DOCKER_PREFIX }},name=myapp
    cache-to: type=s3,region=${{ env.AWS_REGION }},bucket=${{ env.TRUNER_S3_BUCKET }},prefix=${{ env.TRUNER_S3_DOCKER_PREFIX }},name=myapp,mode=max
```

Credentials come from the machine's IAM role. The metadata hop limit is 2, so
BuildKit containers can use it.

## Local NVMe

On instance types with local NVMe storage (`c7gd`, `m6id`, `c6id`, `i4i`, …) truner
stripes all NVMe disks (RAID 0), formats them and places the job work directory
and Docker's data there. Ask for it with `nvme=true`, or pin such a family.
