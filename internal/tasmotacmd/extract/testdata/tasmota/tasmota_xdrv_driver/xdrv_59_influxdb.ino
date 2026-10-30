// reduced from xdrv_59_influxdb.ino: "Prefix||Name": the empty name is the prefix alone ("InfluxDb")
const char kInfluxDbCommands[] PROGMEM = "InfluxDb" "|"  // Prefix
  "|" "Log" "|"
  "Host";

void (* const InfluxCommand[])(void) PROGMEM = {
  &CmndInfluxDbState, &CmndInfluxDbLog,
  &CmndInfluxDbHost };

void CmndInfluxDbState(void) { ResponseCmndStateText(1); }
void CmndInfluxDbLog(void) { ResponseCmndNumber(1); }
void CmndInfluxDbHost(void) { ResponseCmndChar("x"); }

bool Xdrv59(uint32_t function) {
  result = DecodeCommand(kInfluxDbCommands, InfluxCommand);
}
