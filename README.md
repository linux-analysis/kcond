# kcond

kcond answers under which CONFIG_ options the Linux kernel compiles a
file or a change: it evaluates the Kbuild Makefiles of each architecture
build symbolically, reads the `#if` and `IS_ENABLED()` regions around
lines, reads the Kconfig tree to tell which values a symbol can take, and
evaluates the conditions against a `.config`. It is a Go library with a
command line and depends on the standard library and git only.

## Install

    go install github.com/linux-analysis/kcond/cmd/kcond@latest

Or `make build` in a checkout, for `bin/kcond`.

## The tree

A command reads a kernel tree at a git revision (`--tree DIR --rev REV`,
through git's object store, so the checkout stays as it is) or from a
directory (`--src DIR`). The builds are x86_64, i386, arm64, arm,
powerpc, s390, riscv and loongarch; `--build NAME` reads one of them,
otherwise every build the tree has. One build of a recent tree takes
about two seconds to read.

## Questions

`cond` gives the Kbuild condition of files, per build:

    $ kcond --tree ~/linux --rev v6.12 --build x86_64 cond drivers/net/bonding/bond_main.c drivers/net/wireless/intel/iwlwifi/mvm/rx.c
    drivers/net/bonding/bond_main.c	x86_64	built	CONFIG_BONDING
    drivers/net/wireless/intel/iwlwifi/mvm/rx.c	x86_64	built	(CONFIG_IWLMEI || CONFIG_IWLWIFI) && CONFIG_IWLMVM && CONFIG_WLAN && CONFIG_WLAN_VENDOR_INTEL

A file is `built` under its condition, `unlisted` when no Makefile names
it, `not built` when it belongs to another architecture, or a `header`.

`diff` reads commits: the kernel files each one changes, the `#if`
region of the changed lines, each file's condition at the commit, the
condition of the whole change and the architectures that may build it;
with `--config` it tells whether the change applies to that `.config`:

    $ kcond --tree ~/linux diff 8eb36164d1a6
    8eb36164d1a6769a20ed43033510067ff3dab9ee
      drivers/net/bonding/bond_main.c	built	CONFIG_BONDING	region true	arches x86,arm64,arm,powerpc,s390,riscv,loongarch
      drivers/net/bonding/bond_options.c	built	CONFIG_BONDING	region true	arches x86,arm64,arm,powerpc,s390,riscv,loongarch
      include/net/bond_options.h	header		region true	arches x86,arm64,arm,powerpc,s390,riscv,loongarch
      condition: CONFIG_BONDING
      label: generic

`arches` lists the architectures that may build a file, those where its
condition is unknown and those that always build it, and labels the set
`generic`, `several`, one architecture, `other` or `none`:

    $ kcond --tree ~/linux --rev v6.12 arches drivers/net/bonding/bond_main.c arch/arm64/kernel/smccc-call.S
    drivers/net/bonding/bond_main.c	arches x86,arm64,arm,powerpc,s390,riscv,loongarch	unknown 	always 
    arch/arm64/kernel/smccc-call.S	arches arm64	unknown 	always arm64
    label: generic

`symbols` tells what a build's Kconfig says about symbols: the values
they can take (`n`, `m`, `y`), whether they can be modules and which
symbols they require:

    $ kcond --tree ~/linux --rev v6.12 --build x86_64 symbols BONDING NET
    CONFIG_BONDING	x86_64	values nmy	modular true	requires CONFIG_INET,CONFIG_NET,CONFIG_NETDEVICES,CONFIG_NET_CORE
    CONFIG_NET	x86_64	values ny	modular false	requires 

`config` reads a `.config`, plain or gzip-compressed, and `eval` tells
whether conditions hold for it: a condition `applies`, is `not
applicable`, or is `unknown` when an atom no `.config` decides, such as
a macro, is left:

    $ kcond config .config
    build: x86_64
    version: 6.12.0
    partial: false
    options: 1704
    $ kcond --config .config eval CONFIG_NET CONFIG_BONDING CONFIG_NET=m '"defined(FOO)" && CONFIG_NET'
    CONFIG_NET	applies
    CONFIG_BONDING	not applicable
    CONFIG_NET=m	not applicable
    "defined(FOO)" && CONFIG_NET	unknown

Conditions are written `CONFIG_A && (CONFIG_B || !CONFIG_C)`; `CONFIG_X=y`
and `CONFIG_X=m` test one tristate value, a bare `CONFIG_X` either. An
option a `.config` does not list is off. Every command prints JSON with
`--json`.

## Library

```go
import (
	"github.com/linux-analysis/kcond"
	"github.com/linux-analysis/kcond/builds"
	"github.com/linux-analysis/kcond/dotconfig"
	"github.com/linux-analysis/kcond/gitfs"
)

tree, err := gitfs.Open(ctx, dir, "v6.12")  // an fs.FS; os.DirFS works too
b, ok := builds.Named(tree, "x86_64")        // the Makefile crawl and the Kconfig analysis
fc := builds.BuildCond(path, false, kcond.Const(true), b) // Status, Kbuild, Region
e, err := kcond.Parse("CONFIG_A && !CONFIG_B")
f, err := dotconfig.Read(data)               // Config, Version, Build
v := e.Eval(f.Config)                        // kcond.True, False or Unknown
```

`builds.Each` walks every build of a tree; `change.Changes` reads the
files and regions a commit's diff touches, `builds.Combine` and
`builds.Summarize` turn them into one condition and an architecture
label, and `crawl.Resolve` does that for many files across revisions,
crawling each revision once. `kconfig.Parse` and `kconfig.Analyze` give
the values each symbol can take.

## Development

    make lint   # gofmt and go vet
    make test   # go test -race ./...
    make build  # bin/kcond

The module depends on the standard library only; `deps_test.go` checks
it. The tests build small repositories with git;
`kbuild/testdata/kbuild/check.sh` records what the kernel's own Kbuild
makes of the test Makefiles.

## License

MIT, see LICENSE.
