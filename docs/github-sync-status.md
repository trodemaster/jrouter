# jrouter Soft-Seed Mode - Sync Status

**Date**: 2026-01-12  
**GitHub Fork**: https://github.com/trodemaster/jrouter  
**Pull Request**: https://github.com/trodemaster/jrouter/pull/1

## Repository Status ✅

All code is synced to your GitHub fork:

- **Main branch**: `a8c63fb` - Up to date with upstream Gitea
- **Feature branch**: `feature/soft-seed-mode` (commit `1a632da`)
  - 2 commits ahead of main
  - +1,180 lines / -38 lines
  - All changes pushed to GitHub

## Branches on GitHub

```
✅ main                    - a8c63fb (synced)
✅ feature/soft-seed-mode  - 1a632da (synced)
```

## Commits in Feature Branch

1. **0d72a0f** - feat: Add soft-seed router mode configuration and ZIP GetNetInfo client
   - Config structs and protocol documentation
   - ZIP GetNetInfo client implementation

2. **1a632da** - feat: Implement soft-seed and non-seed router modes
   - Core soft-seed implementation
   - Unit tests
   - Documentation updates
   - Integration with main.go

## Files Changed (9 files)

| File | Changes | Description |
|------|---------|-------------|
| `README.md` | +91/-0 | Router mode documentation |
| `docs/implementation-progress.md` | +147/-0 | Development notes |
| `docs/zip-getnetinfo-protocol.md` | +237/-0 | Protocol reference from Netatalk |
| `main.go` | +55/-13 | Router mode initialization |
| `router/config.go` | +41/-3 | Config fields for router modes |
| `router/etalk_port.go` | +113/-27 | Port initialization logic |
| `router/soft_seed.go` | +348/-0 | **Core implementation** |
| `router/soft_seed_test.go` | +177/-0 | Unit tests |
| `router/zip.go` | +9/-1 | Skip GetNetInfo in non-seed mode |

## Test Results ✅

### Unit Tests
```bash
$ go test ./...
✅ All tests pass
```

### Integration Test (MacPro 2013)
```bash
$ getzones
✅ netjibbing + 27 AURP zones

$ nbplkup
✅ macpro2013:AFPServer 650.37:128
✅ jrouter v0.0.21-dev:AppleRouter 650.1:253
```

### Configuration Used
- **atalkd** (seed router): `enp12s0 -router -phase 2 -net 650 -addr 650.37 -zone "netjibbing"`
- **jrouter** (soft-seed): `router_mode: soft-seed` on `enp11s0`
- **Netatalk**: Successfully serving AFP shares

## Pull Request

**URL**: https://github.com/trodemaster/jrouter/pull/1  
**Status**: Open, ready for review  
**Title**: feat: Implement soft-seed and non-seed router modes  

## Next Steps for Upstream

The upstream repository is on Gitea (`gitea.drjosh.dev/josh/jrouter`). To submit:

**Option 1: Email to maintainer**
```
To: josh.deprez@gmail.com
Subject: [jrouter] Soft-seed mode implementation

Hi Josh,

I've implemented soft-seed and non-seed router modes for jrouter,
enabling it to coexist with Netatalk's atalkd on the same network.

Pull Request: https://github.com/trodemaster/jrouter/pull/1
Issue: https://gitea.drjosh.dev/josh/jrouter/issues/7

Would you be interested in reviewing this for inclusion?

Best regards,
Blake (trodemaster)
```

**Option 2: Create Gitea PR**
1. Fork jrouter on Gitea
2. Push your branch there
3. Create PR via Gitea web interface

**Option 3: GitHub Discussion**
- The maintainer might monitor GitHub forks
- PR is public and ready for feedback

## Notes

- Binary `jrouter-softseed` is built locally (not committed)
- All configuration files updated in `/home/blake/code/machine-cfg/macpro2013/`
- Services running successfully on MacPro 2013
- No merge conflicts with upstream main
