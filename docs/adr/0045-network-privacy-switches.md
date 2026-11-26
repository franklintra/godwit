---
status: proposed
---

# Network privacy: a switch for every connection Godwit makes by itself

Besides mail, Godwit opens connections nobody asked for at that moment: it asks dl.godwit.email for a newer version, refreshes the plugin directory there, fetches models.dev's catalog of model names, and fetches sender avatars from gravatar.com, the sender's website and the logo its domain publishes in DNS (BIMI), whose mark certificate the security scan also fetches. Only the avatars had a control, and only through 远程图片 (RemoteImages), which also governs the mail's own images. The update check was switchable only in the updater's own updater.json, and the catalog and the VMC fetch ignored the proxy set in Godwit.

A user who wants to know what leaves the machine had no single place to look, and no way to stop one of these without stopping the others.

## Decision

- **One switch per destination, in Settings.** `ModelCatalog`, `PluginRegistry`, `Gravatar`, `Favicons`, `BIMI` and `UpdateChecks`, plain booleans in `Settings`, on by default: nothing changes until the user turns one off. A new page, 隐私与安全 › 网络隐私 (Network privacy), lists each with the host it reaches, and links to 远程图片 and 网络代理 for what is set there.
- **A switch stops what nobody asked for; an explicit request still goes.** Off, models.dev is never asked (the cached catalog stays in use); the plugin directory is not refreshed in the background nor as 插件 opens, but `RefreshPluginRegistry` and the directory's 立即更新 still fetch it; the app does not ask the update feed by itself, but 检查更新 does. Turning a switch on fetches what was skipped at once.
- **Avatar sources keep the remote-image policy.** A source loads only where RemoteImages lets the message's images load *and* its switch is on. A source switched off is neither asked nor read from the image cache, so what the user sees matches the switch.
- **BIMI covers both of its fetches.** The avatar's DNS logo and the scan's VMC fetch go to hosts the sender's DNS names; with BIMI off neither runs, and no new mail is marked a certified brand.
- **The backend keeps the switches in memory** (`netSwitches`), set at `New` and by `SetSettings`, so the hourly catalog loop and every avatar read no database.
- **`UpdateChecks` lives in Settings, and the app keeps the updater's file in step.** The mygo updater runs in the interface process and keeps its own choice in updater.json; the platform writes `updater.SetAutomaticChecks` whenever the setting changes. At start, a file whose automatic checks were turned off by hand before this setting existed wins: the setting is turned off to match, so no upgrade turns checks back on. The Store's package has no updater; there the switch stops the Store catalog check.
- **Every one of these goes through the proxy set in Godwit.** The catalog, the registry (and its downloads and READMEs) and the Store's catalog use `proxyClient`, an `http.Transport` whose proxy is `Service.Proxy` and which lets the proxy resolve the name, as `StoreLatestVersion` already did. The VMC fetch, whose URL the sender chooses, uses the image client's transport (imagefetch: public addresses only, through the proxy), as avatars and images do. DNS lookups for BIMI still use the system resolver.
- **`GODWIT_OFFLINE_DEFAULTS=1`** in the backend's environment makes the six switches default off. A setting saved since keeps what it holds.

## Consequences

- With `PluginRegistry` off, a reviewed plugin the reviewers later withdraw keeps running until the directory is refreshed by hand: the directory is also the revocation list (ADR 0016). With no plugin from the directory installed, nothing is lost. The page says so under the switch.
- With `BIMI` off, mail already marked a certified brand keeps the mark until it is scanned again.
- A Local API client that writes Settings from an older snapshot, without these fields, turns them off (a JSON `false`), as with every boolean added before; clients read, change and write the whole object.
