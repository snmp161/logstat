# Cutting a release

Written down because this project is in maintenance mode: by the time the next
release comes around, nobody will remember the parts that are not automated.

Everything below the horizontal rules is automated — `release.yml` fires on a
`v*` tag, runs lint and the race suite, and publishes the GitHub Release through
GoReleaser. What follows is the part a person has to do.

## Before the tag

1. **CI is green on the commit you are about to tag**, including `install (deb +
   systemd)` and `install (rpm)`. Those two jobs are the only place the
   maintainer scripts and the unit file ever run; the release workflow does not
   repeat them.
2. **Soak it for a day.** `DURATION=24h RESET='0 * * * *' make soak` on a host
   with a real Redis. The suite never runs longer than half a minute and fakes
   the clock, so this is the only check that a reset really fires, a TTL really
   holds and memory really stays flat. Read the closing checklist the script
   prints.
3. **Pick the version by what changed.** New options or new behaviour that keeps
   existing configs working is a minor bump; a fix on its own is a patch. The
   options logstat adds are expected to default to the previous behaviour, so a
   major bump should be a surprise — if you find yourself needing one, say why in
   the upgrade notes.
4. **Write the upgrade notes.** The `release.header` in `.goreleaser.yaml` is
   permanent and cumulative: add a paragraph for the version you are cutting,
   worded from the reader's side ("Coming from v0.3.0?"), and leave the older
   ones in place. Whichever release page someone lands on should tell them what
   an upgrade does to their Redis keys and their config.
5. **Check the documentation matches the code**, in both READMEs and in
   `docs/specification.md`. The parameter tables, the metric table and the
   defaults are the parts that drift.

## The tag

```sh
git tag -a v0.4.0 -m "v0.4.0"
git push origin v0.4.0
```

Nothing else: pushing the tag is what publishes the release.

## After the release page appears

1. **The artifacts are all there**: two archives, four packages
   (`logstat_<version>-1_{amd64,arm64}.deb`,
   `logstat-<version>-1.{x86_64,aarch64}.rpm`) and `checksums.txt`.
2. **The install snippet in the release body names files that exist.** The
   package names come from `file_name_template` in `.goreleaser.yaml`; CI asserts
   the deb name, but the snippet itself is prose and nothing checks it.
3. **Install one for real.** Download the `.deb` from the release page onto a
   scratch host or container, install it, and confirm `logstat version` prints
   the tag you just pushed. It takes two minutes and it is the only test of the
   artifact people will actually download.

If something is wrong, delete the release and the tag, fix it, and push the tag
again — GoReleaser recreates the release from scratch.

## What is deliberately not covered

The `.rpm` is only ever installed in a container, so its scriptlets are checked
but its unit is never started by systemd. If you have an RPM-based host around,
that is the gap worth closing by hand.
