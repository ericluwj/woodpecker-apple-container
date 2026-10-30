# Repository operator rules

- Do not run `git push` to `main`. Prepare local commits and let the user push them.
- Publishing releases, release assets, and moving release aliases must be done exclusively by GitHub Actions. Do not publish them from a local shell or through local API calls.
- Local publishing tests must use fake GitHub APIs and must not mutate real releases or tags.
