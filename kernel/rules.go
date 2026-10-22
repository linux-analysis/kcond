package kernel

// ArchDirs are the directories under arch/<arch> that the arch's own rules
// build in every configuration though no list names them: arch/x86/Makefile
// builds the boot image and the relocs tool, realmode/Makefile the
// real-mode blob.
var ArchDirs = map[string][]string{"x86": {"boot", "boot/compressed", "realmode/rm", "tools"}}

// OwnList is an object list a Makefile links with its own rules. A gated
// list is linked into one of the gate objects, which are built when obj-y
// lists them, as the x86 vDSO's objects are linked into its images.
type OwnList struct {
	Name string
	Gate []string
}

// vdso64 are the images of the 64-bit and x32 vDSOs, vdso.o and vdsox32.o
// before v3.16.
var vdso64 = []string{"vdso-image-64.o", "vdso-image-x32.o", "vdso.o", "vdsox32.o"}

// OwnLists name, by directory, the object lists a Makefile links with its
// own rules rather than through obj-y; they are read like obj-y. Their
// words may carry the $(obj)/ prefix those rules need.
var OwnLists = map[string][]OwnList{
	"arch/x86/boot":              {{Name: "setup-y"}},
	"arch/x86/boot/compressed":   {{Name: "vmlinux-objs-y"}, {Name: "VMLINUX_OBJS"}},
	"arch/x86/realmode/rm":       {{Name: "realmode-y"}},
	"arch/x86/purgatory":         {{Name: "purgatory-y"}},
	"arch/x86/vdso":              {{"vobjs-y", vdso64}},
	"arch/x86/entry/vdso":        {{"vobjs-y", vdso64}, {"vobjs32-y", []string{"vdso-image-32.o"}}},
	"arch/x86/entry/vdso/vdso64": {{Name: "vobjs-y"}},
	"arch/x86/entry/vdso/vdso32": {{Name: "vobjs-y"}},
	// The vDSOs of the other label arches, under their directories'
	// conditions; powerpc's 32- and 64-bit lists share one from v6.1 on,
	// and its C objects are named in targets alone.
	"arch/arm64/kernel/vdso":        {{Name: "obj-vdso"}},
	"arch/arm64/kernel/vdso32":      {{Name: "obj-vdso"}},
	"arch/arm/vdso":                 {{Name: "obj-vdso"}},
	"arch/riscv/kernel/vdso":        {{Name: "obj-vdso"}},
	"arch/riscv/kernel/compat_vdso": {{Name: "obj-compat_vdso"}},
	"arch/loongarch/vdso":           {{Name: "obj-vdso"}},
	"arch/powerpc/kernel/vdso":      {{Name: "obj-vdso32"}, {Name: "obj-vdso64"}, {Name: "targets"}},
	"arch/powerpc/kernel/vdso32":    {{Name: "obj-vdso32"}},
	"arch/powerpc/kernel/vdso64":    {{Name: "obj-vdso64"}},
	"arch/s390/kernel/vdso":         {{Name: "obj-vdso"}, {Name: "obj-cvdso"}},
	"arch/s390/kernel/vdso32":       {{Name: "obj-vdso32"}},
	"arch/s390/kernel/vdso64":       {{Name: "obj-vdso64"}, {Name: "obj-cvdso64"}},
	// arm64's nVHE hypervisor compiles its list to %.nvhe.o objects and
	// links them into kvm_nvhe.o, which obj-y lists.
	"arch/arm64/kvm/hyp/nvhe": {{"hyp-obj-y", []string{"kvm_nvhe.o"}}},
}

// Wrappers are objects that stand for a directory the arch's own rules
// build: arm64's vdso-wrap.S includes vdso/vdso.so, which the arch
// Makefile builds before the kernel, and no list names vdso/. The build
// enters the directory under the object's condition.
var Wrappers = map[string]map[string]string{
	"arch/arm64/kernel":   {"vdso-wrap.o": "vdso", "vdso32-wrap.o": "vdso32"},
	"arch/powerpc/kernel": {"vdso32_wrapper.o": "vdso", "vdso64_wrapper.o": "vdso"},
}
