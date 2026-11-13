package config

import "errors"

// The errors below are the reasons a configuration is refused. Each carries only the reason;
// the code that finds the problem adds the key and the value in front, so that the message
// an administrator reads names what to fix. Callers test for a reason with errors.Is.
var (
	// ErrInvalidRate is the reason a rate is not written as <count>/<s|m|h>.
	ErrInvalidRate = errors.New("use <count>/<s|m|h>, for example 10/m")
	// ErrMultipleDocuments is the reason a file with a second YAML document is refused.
	ErrMultipleDocuments = errors.New("more than one YAML document")
	// ErrBasePathNoSlash is the reason a base path that does not start with a slash is refused.
	ErrBasePathNoSlash = errors.New(`must start with "/"`)
	// ErrEmpty is the reason a required value is refused.
	ErrEmpty = errors.New("must not be empty")
	// ErrNotOneOf is the reason a value outside a fixed list is refused. The list follows it.
	ErrNotOneOf = errors.New("is not one of")
	// ErrDurationTooShort is the reason an unlimited or too short duration is refused. The
	// shortest allowed duration follows it.
	ErrDurationTooShort = errors.New("is not allowed, use a finite duration of at least")
	// ErrNoListener is the reason a configuration without a listener is refused.
	ErrNoListener = errors.New("listen: at least one entry is required")
	// ErrPortOutOfRange is the reason a listener port that is not a TCP port is refused.
	ErrPortOutOfRange = errors.New("is not between 1 and 65535")
	// ErrDuplicateListener is the reason a second listener on the same address and port is
	// refused. The position of the first one follows it.
	ErrDuplicateListener = errors.New("is already listed in")
	// ErrTLSPairIncomplete is the reason a TLS block with only one of its two files is refused.
	ErrTLSPairIncomplete = errors.New("cert_file and key_file must both be set or both be left out")
	// ErrPlainWithTLS is the reason a listener that is both plain and TLS is refused.
	ErrPlainWithTLS = errors.New("must not be true together with tls")
	// ErrNamePattern is the reason a share or node name of the wrong form is refused. The
	// pattern follows it.
	ErrNamePattern = errors.New("must match")
	// ErrExcludeSlash is the reason an exclude pattern with a slash is refused.
	ErrExcludeSlash = errors.New(`must not contain "/"`)
	// ErrCountBelowOne is the reason a login rate that allows nothing is refused.
	ErrCountBelowOne = errors.New("is not at least 1")
	// ErrURLInvalid is the reason a cluster URL that clients cannot use is refused.
	ErrURLInvalid = errors.New(
		"must be an absolute http or https URL with a host, without query or fragment")
	// ErrPathNotAbsolute is the reason a share path that is not absolute is refused.
	ErrPathNotAbsolute = errors.New("must be absolute")
)
