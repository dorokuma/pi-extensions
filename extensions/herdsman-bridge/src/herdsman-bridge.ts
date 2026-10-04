// Thin bridge: loads the Herdsman pi extension from the local workspace package.
// Installed by worker (2026-08-26) so every pi session loads the herdsman
// back-channel extension. The implementation lives in
// /root/workspace/herdsman/packages/herdsman-pi/src/index.ts — its default export is the
// ready-to-use extension function (the createHerdsmanPiExtension() instance),
// loaded directly as TypeScript by pi's jiti-based extension loader, so no
// copy of the package is needed.
//
// NOTE: this mirrors what the package's own "pi.extensions": ["./src/index.ts"]
// manifest declares; it is kept as the single registration point so the
// settings.json "packages" entry for the local path was removed to avoid
// double-loading the extension (two daemon clients / duplicate registrations).

export { default } from "/root/workspace/herdsman/packages/herdsman-pi/src/index.ts";