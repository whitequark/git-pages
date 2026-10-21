package names

type NameError struct {
	text string
}

func (err NameError) Error() string {
	return err.text
}
