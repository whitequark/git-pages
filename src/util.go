package git_pages

import (
	"errors"
	"io"
	"strings"

	"google.golang.org/protobuf/proto"
)

type BoundedReader struct {
	inner io.Reader
	fuel  int64
	err   error
}

func ReadAtMost(reader io.Reader, count int64, err error) io.Reader {
	return &BoundedReader{reader, count, err}
}

func (reader *BoundedReader) Read(dest []byte) (count int, err error) {
	if reader.fuel <= 0 {
		return 0, reader.err
	}
	if int64(len(dest)) > reader.fuel {
		dest = dest[0:reader.fuel]
	}
	count, err = reader.inner.Read(dest)
	reader.fuel -= int64(count)
	return
}

type prettyError interface {
	error
	Pretty() string
}

func prettyErrMsg(err error) string {
	switch cerr := err.(type) {
	case prettyError:
		return cerr.Pretty()
	default:
		return cerr.Error()
	}
}

type prettyJoinError struct {
	errs []error
}

func joinErrors(errs ...error) error {
	if err := errors.Join(errs...); err != nil {
		wrapErr := err.(interface{ Unwrap() []error })
		return &prettyJoinError{errs: wrapErr.Unwrap()}
	}
	return nil
}

func (e *prettyJoinError) Error() string {
	var s strings.Builder
	for i, err := range e.errs {
		if i > 0 {
			s.WriteString("; ")
		}
		s.WriteString(err.Error())
	}
	return s.String()
}

func (e *prettyJoinError) Pretty() string {
	var s strings.Builder
	for i, err := range e.errs {
		if i > 0 {
			s.WriteString("\n- ")
		}
		s.WriteString(strings.ReplaceAll(prettyErrMsg(err), "\n", "\n  "))
	}
	return s.String()
}

func (e *prettyJoinError) Unwrap() []error {
	return e.errs
}

func getMediaType(mimeType string) (mediaType string) {
	mediaType, _, _ = strings.Cut(mimeType, ";")
	mediaType = strings.TrimSpace(strings.ToLower(mediaType))
	return
}

// Copying Protobuf messages like `*dest = *src` causes a lock to be copied, which is unsound.
// Copying Protobuf messages field-wise is fragile: adding a new field to the schema does not
// cause a diagnostic to be emitted pointing to the copy site, making it easy to miss updates.
// Serializing and deserializing is reliable and breaks referential links.
func CopyProtoMessage(dest, src proto.Message) {
	data, err := proto.Marshal(src)
	if err != nil {
		panic(err)
	}

	err = proto.Unmarshal(data, dest)
	if err != nil {
		panic(err)
	}
}
