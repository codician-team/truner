# truner documentation

These pages are also published as the documentation website (built with
[VitePress](https://vitepress.dev) from this folder).

- [Getting started](getting-started.md)
- [Install with an AI agent](ai-agents.md)
- [FAQ](faq.md)
- [Labels](labels.md)
- [Configuration](configuration.md)
- [Caching](caching.md)
- [Images](images.md)
- [Warm pools](pools.md)
- [MicroVMs (beta)](microvms.md)
- [Parallel jobs](parallel-jobs.md)
- [Networking and access](networking.md)
- [CLI reference](cli.md)
- [Costs and limits](costs.md)
- [Troubleshooting](troubleshooting.md)
- [Security](../SECURITY.md)
- [Architecture](architecture.md)
- [Publishing (maintainers)](publishing.md)

## Working on the website

```bash
cd docs
npm ci
npm run dev       # http://localhost:5173 with live reload
npm run build     # static site in docs/.vitepress/dist
```

`index.md` is the landing page. Its sections are Vue components in
`.vitepress/theme/components/landing/`. Every other page is plain Markdown that also
reads well on GitHub.
