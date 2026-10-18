# One object in obj-y and obj-m.
# configs: CONFIG_A=y CONFIG_B=m;CONFIG_A=m CONFIG_B=y;CONFIG_A=m CONFIG_B=m
obj-$(CONFIG_A) += x.o
obj-$(CONFIG_B) += x.o
