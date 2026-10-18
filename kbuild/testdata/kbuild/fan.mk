# A member of a composite under its own option, as drivers/acpi's fan.
# configs: CONFIG_A=y CONFIG_B=y;CONFIG_A=y CONFIG_B=m;CONFIG_A=m CONFIG_B=y;CONFIG_A=m CONFIG_B=m
obj-$(CONFIG_A) += fan.o
fan-objs := fan_core.o
fan-$(CONFIG_B) += fan_hwmon.o
