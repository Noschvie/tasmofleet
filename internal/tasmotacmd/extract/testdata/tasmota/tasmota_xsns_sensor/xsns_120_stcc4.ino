// reduced from xsns_120_stcc4.ino: upstream has one handler fewer than names (a commented-out entry
// elsewhere); the extractor must not pair by guessing
const char kStccCommands[] PROGMEM = "Stcc|"  // Prefix
  "|Pres|RHT|Cal|Reset";

void (* const StccCommand[])(void) PROGMEM = {
  &CmndStccState, &CmndStccPressure, &CmndStccRHT, &CmndStccReset };

void CmndStccState(void) { ResponseCmndNumber(1); }

bool Xsns120(uint32_t function) {
  result = DecodeCommand(kStccCommands, StccCommand);
}
