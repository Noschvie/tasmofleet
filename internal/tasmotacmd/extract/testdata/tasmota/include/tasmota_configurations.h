// excerpt of tasmota/include/tasmota_configurations.h: a variant switches defines off
#ifdef FIRMWARE_LITE
#undef USE_SUNRISE                               // Disable support for Sunrise and sunset tools
#endif  // FIRMWARE_LITE
#ifdef FIRMWARE_MINIMAL
#undef USE_SUNRISE
#endif
