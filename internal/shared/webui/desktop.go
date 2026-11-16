package webui

// The restart and stop pair of the Desktop section, polled at PollURL so
// a warning reads what is running when it is clicked.
type DesktopPower struct {
	Name           string
	PollURL        string
	RestartWarning string
	QuitWarning    string
	CSRFToken      string
}
