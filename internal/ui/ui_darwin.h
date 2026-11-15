#ifndef K8S_BUDDY_UI_DARWIN_H
#define K8S_BUDDY_UI_DARWIN_H

void buddy_run(const char *characterURL, const char *panelURL, const char *overviewURL, int showCharacter, int inspectable);
void buddy_set_tray(const char *title, const char *tooltip);
void buddy_show_overview(const char *tab);
void buddy_toggle_panel(void);
void buddy_hide_panel(void);
void buddy_set_character_visible(int visible);
void buddy_quit(void);

#endif
