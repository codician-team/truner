# Warm pools

A pool keeps a number of machines **fully prepared and stopped**. A stopped instance
costs only its EBS volume. When a job asks for the pool, truner hands the stopped machine
the job's runner configuration and starts it. It is running the job in about 10–20 seconds
instead of 30–60.

```yaml
# truner config edit
pools:
  fast:
    runner: 4cpu-linux-x64   # any profile, plus overrides:
    disk: 60
    size: 3                  # machines kept ready
    maxAgeHours: 72          # recycle machines older than this
```

```yaml
runs-on: truner/pool=fast/run=${{ github.run_id }}
```

- The janitor (every 2 minutes) launches machines until `size` are ready. Each one
  installs its software and runner, reports success and stops.
- A pool machine runs one job and is then terminated like any other machine. A
  fresh one replaces it.
- If the pool is empty, the job gets a regular machine with the same spec, so it
  never waits for the pool.
- Changing a pool's definition or upgrading truner recycles its machines.
- Pool machines are on-demand, because stopped spot instances are not supported.
  Only Linux images can be pooled.
- `keep`, `ssh`, `timeout` and `cache` labels still apply. Hardware labels are ignored.

`truner pools` shows each pool's ready, preparing and in-use machines.

**Cost:** a ready machine costs its stopped EBS volume, e.g. 40 GiB gp3 ≈ $3.20 per
month, plus the few minutes it ran while being prepared.
