package broker

import (
	"bufio"
	"context"
	"net"
)

type contextT = context.Context

func bufioReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }
