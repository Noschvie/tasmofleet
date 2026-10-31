// excerpt of tasmota/my_user_config.h: the defines of the default build
#define USE_SUNRISE                              // Add support for Sunrise and sunset tools (+16k)
#define USE_WEBCAM
#ifdef ESP8266
  #undef USE_WEBCAM
#endif
//#define USE_COMMENTED_OUT
#ifdef USE_SUNRISE
  #define USE_SUNRISE_EXTRA
#endif
