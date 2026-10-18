# A member forced into the composite whatever its option, as net/bridge.
# configs: CONFIG_A=y CONFIG_B=m;CONFIG_A=m CONFIG_B=m
obj-$(CONFIG_A) += bridge.o
bridge-y := br.o
bridge-$(subst m,y,$(CONFIG_B)) += br_nf_core.o
