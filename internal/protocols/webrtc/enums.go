package webrtc

// EnableTwoWayAudioType defines the enum for enabling two-way audio.
type EnableTwoWayAudioType int

const (
	DisableTwoWayAudio EnableTwoWayAudioType = iota
	UnifiCameras
	DahuaCameras
	HikvisionCameras
)

// String returns the string representation of the EnableTwoWayAudioType.
func (e EnableTwoWayAudioType) String() string {
	return [...]string{"DisableTwoWayAudio", "UnifiCameras", "DahuaCameras", "HikvisionCameras"}[e]
}
