// excerpt of tasmota/include/i18n.h
#define D_JSON_DONE "Done"
#define D_JSON_NAME "NAME"
#define D_JSON_ARCH "ARCH"
#define D_JSON_GPIO "GPIO"
#define D_JSON_FLAG "FLAG"
#define D_JSON_BASE "BASE"
#define D_JSON_INVALID_JSON "Invalid JSON"

#define D_CMND_BACKLOG "Backlog"
#define D_CMND_OTAURL "OtaUrl"
#define D_CMND_POWER "Power"
#define D_CMND_STATUS "Status"
#define D_CMND_TELEPERIOD "TelePeriod"
#define D_CMND_SO "SO"
#define D_CMND_SETOPTION "SetOption"
#define D_CMND_TEMPLATE "Template"
#define D_CMND_GPIO "GPIO"
#define D_CMND_MODULE "Module"
#define D_CMND_FRIENDLYNAME "FriendlyName"
#define D_SO_WIFINOSLEEP "WifiNoSleep"
#define D_PRFX_MQTT "Mqtt"
#define D_CMND_MQTTHOST "Host"

const char S_JSON_COMMAND_NVALUE[] PROGMEM =                  "{\"%s\":%d}";
const char S_JSON_COMMAND_SVALUE[] PROGMEM =                  "{\"%s\":\"%s\"}";
const char S_JSON_COMMAND_INDEX_NVALUE[] PROGMEM =            "{\"%s%d\":%d}";
const char S_JSON_COMMAND_INDEX_SVALUE[] PROGMEM =            "{\"%s%d\":\"%s\"}";
const char S_JSON_COMMAND_NVALUE_SVALUE[] PROGMEM =           "{\"%s\":{\"%d\":\"%s\"}}";
