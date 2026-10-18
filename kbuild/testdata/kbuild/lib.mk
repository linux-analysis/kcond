# A library object, linked into the kernel either way.
# configs: CONFIG_L=y;CONFIG_L=m
lib-$(CONFIG_L) += l.o
