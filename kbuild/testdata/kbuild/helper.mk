# A helper list read by its -y name only.
# configs: CONFIG_C=y;CONFIG_C=m
mmu-$(CONFIG_C) := nommu_free.o
obj-y += core.o $(mmu-y)
