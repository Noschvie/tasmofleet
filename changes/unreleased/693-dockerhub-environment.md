---
section: Security
---
Docker Hub credentials are read only through the `dockerhub` deployment environment (policy: `main` and `v*` tags); pull requests and other branches never receive them, and missing credentials on `main` or a release tag now fail the job instead of silently skipping Docker Hub (#693)
