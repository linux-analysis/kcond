# drivers/platform/x86/intel's target lists, made into obj- lines by eval.
# configs: CONFIG_X=y;CONFIG_X=m
intel-target-$(CONFIG_X) += hid.o
define INTEL_OBJ_TARGET
intel-$(1)-y := $(1).o
obj-$(2) += intel-$(1).o
endef
$(foreach target, $(basename $(intel-target-y)), $(eval $(call INTEL_OBJ_TARGET,$(target),y)))
$(foreach target, $(basename $(intel-target-m)), $(eval $(call INTEL_OBJ_TARGET,$(target),m)))
