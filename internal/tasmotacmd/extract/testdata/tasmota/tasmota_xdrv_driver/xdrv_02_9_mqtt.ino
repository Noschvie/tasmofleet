// excerpt of xdrv_02_9_mqtt.ino: a prefix table, the typed command is prefix + name
const char kMqttCommands[] PROGMEM = D_PRFX_MQTT "|"  // Prefix
  D_CMND_MQTTHOST;

void (* const MqttCommand[])(void) PROGMEM = {
  &CmndMqttHost };

void CmndMqttHost(void) {
  if (XdrvMailbox.data_len > 0) {
    SettingsUpdateText(SET_MQTT_HOST, XdrvMailbox.data);
  }
  ResponseCmndChar(SettingsText(SET_MQTT_HOST));
}

bool Xdrv02(uint32_t function) {
  result = DecodeCommand(kMqttCommands, MqttCommand);
}
