// reduced from xdrv_39_thermostat.ino: #ifdef/#else alternatives inside both lists; only inside one
// configuration do the name list and the handler list have the same length
#ifdef USE_THERMOSTAT
const char kThermostatCommands[] PROGMEM = "|" "ModeSet" "|" "ClimateSet" "|"
#ifdef USE_PI_AUTOTUNING
  "AntiWindup" "|" "PerfLevel" "|" "MaxAction" "|"
#else
  "AntiWindup" "|" "MaxAction" "|"
#endif // USE_PI_AUTOTUNING
  "EnableOutput";

void (* const ThermostatCommand[])(void) PROGMEM = {
  &CmndModeSet, &CmndClimateSet,
#ifdef USE_PI_AUTOTUNING
  &CmndAntiWindup, &CmndPerfLevel, &CmndMaxAction,
#else
  &CmndAntiWindup, &CmndMaxAction,
#endif // USE_PI_AUTOTUNING
  &CmndEnableOutput };

void CmndModeSet(void) { ResponseCmndNumber(1); }
void CmndClimateSet(void) { ResponseCmndNumber(1); }
void CmndAntiWindup(void) { ResponseCmndNumber(1); }
void CmndPerfLevel(void) { ResponseCmndNumber(1); }
void CmndMaxAction(void) { ResponseCmndNumber(1); }
void CmndEnableOutput(void) { ResponseCmndNumber(1); }
#endif

bool Xdrv39(uint32_t function) {
  result = DecodeCommand(kThermostatCommands, ThermostatCommand);
}
