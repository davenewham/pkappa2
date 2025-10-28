#!/usr/bin/env python3
import traceback
import zlib
from base64 import b64encode
from collections import defaultdict
from dataclasses import dataclass
from hashlib import sha1
from typing import Any, Dict, List, Optional, Union

import hyperframe.frame

from http2 import HeaderTuple, HTTP2Converter, HTTPRequest, HTTPResponse
from pkappa2lib import Direction, Result, Stream, StreamChunk


@dataclass
class WebsocketFrame:
    Direction: Direction
    Header: bytearray
    Data: bytearray


class WebsocketConverter(HTTP2Converter):
    websocket_key: Union[bytes, None]
    switched_protocols: bool
    websocket_remaining_data: Dict[int, Dict[Direction, bytes]]
    websocket_deflate: Dict[int, bool]
    websocket_deflate_decompressor: Dict[int, Dict[Direction, Any]]
    websocket_message_fragmented_frames: Dict[int, List[WebsocketFrame]]
    websocket_enable_connect_protocol: bool
    websocket_stream: Dict[int, bool]

    def __init__(self):
        super().__init__()
        self.SETTINGS_NAMES.update(
            {
                8: "ENABLE_CONNECT_PROTOCOL",
            }
        )

    def unmask_websocket_frames(self, frame: WebsocketFrame) -> WebsocketFrame:
        # this frame is unmasked
        if frame.Header[1] & 0x80 == 0:
            return frame

        unmasked = [
            frame.Data[4 + i] ^ frame.Data[i % 4] for i in range(len(frame.Data) - 4)
        ]
        # remove mask bit
        frame.Header[1] = frame.Header[1] & 0x7F
        return WebsocketFrame(frame.Direction, frame.Header, bytearray(unmasked))

    def handle_websocket_permessage_deflate(
        self, stream_id: int, frame: WebsocketFrame
    ) -> Optional[WebsocketFrame]:
        opcode = frame.Header[0] & 0x0F
        # control frames are not compressed
        if opcode & 0x08 != 0:
            return frame

        # handle fragmented messages
        if frame.Header[0] & 0x80 == 0:  # FIN bit not set
            self.websocket_message_fragmented_frames[stream_id].append(frame)
            if len(self.websocket_message_fragmented_frames[stream_id]) > 1 and opcode != 0:
                del self.websocket_message_fragmented_frames[stream_id]
                raise Exception("Invalid fragmented message")
            if len(self.websocket_message_fragmented_frames[stream_id]) > 50:  # arbitrary limit
                del self.websocket_message_fragmented_frames[stream_id]
                raise Exception("Fragmented message too long")
            return None

        if len(self.websocket_message_fragmented_frames[stream_id]) > 0:
            if opcode != 0:
                del self.websocket_message_fragmented_frames[stream_id]
                raise Exception("Invalid fragmented message")
            # this is the last frame of a fragmented message
            self.websocket_message_fragmented_frames[stream_id].append(frame)
            frame = WebsocketFrame(
                Direction=frame.Direction,
                Header=self.websocket_message_fragmented_frames[stream_id][0].Header,
                Data=bytearray(
                    b"".join(
                        [
                            f.Data
                            for f in self.websocket_message_fragmented_frames[stream_id]
                        ]
                    )
                ),
            )
            frame.Header[0] |= 0x80  # set FIN bit
            # datalength in the header is wrong now, but we don't care
            del self.websocket_message_fragmented_frames[stream_id]

        # only the first frame of a fragmented message has the RSV1 bit set
        if frame.Header[0] & 0x40 == 0:  # RSV1 "Per-Message Compressed" bit not set
            return frame

        data = frame.Data + b"\x00\x00\xff\xff"
        data = self.websocket_deflate_decompressor[stream_id][
            frame.Direction
        ].decompress(data)
        frame.Header[0] = frame.Header[0] & 0xBF  # remove RSV1 bit
        return WebsocketFrame(frame.Direction, frame.Header, bytearray(data))

    def handle_websocket_frame(self, frame: WebsocketFrame) -> WebsocketFrame:
        """
        Handle a websocket frame and possibly return a new frame.
        Use this to implement custom websocket protocols.

        Args:
            chunk (StreamChunk): The chunk that contains the frame
            frame (WebsocketFrame): The frame to handle

        Returns:
            WebsocketFrame: The frame to send to display. Header and Body concatenated.
        """
        return frame

    def handle_websocket_frames(
        self, direction: Direction, stream_id: int, content: bytes
    ) -> bytes:
        try:
            frames: List[bytes] = []
            frame = bytearray(
                self.websocket_remaining_data[stream_id][direction] + content
            )
            del self.websocket_remaining_data[stream_id][direction]
            while len(frame) > 1:
                try:
                    data_length = frame[1] & 0x7F
                    mask_offset = 2
                    if data_length == 126:
                        mask_offset = 4
                    elif data_length == 127:
                        mask_offset = 10

                    if len(frame) < mask_offset:
                        raise IndexError("Not enough data for a full frame")
                    if mask_offset > 2:
                        data_length = int.from_bytes(
                            frame[2:mask_offset], byteorder="big"
                        )

                    data_offset = mask_offset
                    # frame masked?
                    if frame[1] & 0x80 != 0:
                        data_offset += 4

                    if len(frame) < data_offset + data_length:
                        raise IndexError("Not enough data for a full frame")

                    websocket_frame = WebsocketFrame(
                        Direction=direction,
                        Header=frame[:mask_offset],
                        Data=frame[mask_offset : data_offset + data_length],
                    )
                    websocket_frame = self.unmask_websocket_frames(websocket_frame)
                    if self.websocket_deflate[stream_id]:
                        deflated_websocket_frame = self.handle_websocket_permessage_deflate(
                            stream_id, websocket_frame
                        )
                        if deflated_websocket_frame is None:
                            break
                        websocket_frame = deflated_websocket_frame
                    websocket_frame = self.handle_websocket_frame(websocket_frame)

                    frames.append(websocket_frame.Header + bytes(websocket_frame.Data))
                    frame = frame[data_offset + data_length :]
                except IndexError:
                    # not enough data for a full frame
                    break
            if len(frame) > 0:
                # TODO: For http/2 the last chunk doesn't necessarily have to contain
                #       data for a websocket frame, so remaining data could be silently
                #       hidden. This is a general problem with truncated http/2 traffic.
                if self.is_last_chunk:
                    frames.append(frame)
                else:
                    self.websocket_remaining_data[stream_id][direction] = frame
            return b"".join(frames)
        except Exception as ex:
            self.log(f"Error while handling websocket frame: {ex}")
            self.log(traceback.format_exc())

            raise Exception(f"Error while handling websocket frame: {ex}") from ex

    def handle_permessage_deflate_extension(
        self, stream_id: int, websocket_deflate_parameters: Dict[str, Union[bool, str]]
    ) -> None:
        self.websocket_deflate[stream_id] = True
        self.websocket_message_fragmented_frames[stream_id] = []
        window_bits = 15
        if "server_max_window_bits" in websocket_deflate_parameters:
            window_bits = int(websocket_deflate_parameters["server_max_window_bits"])
        self.websocket_deflate_decompressor[stream_id][Direction.SERVERTOCLIENT] = (
            zlib.decompressobj(wbits=-window_bits)
        )
        window_bits = 15
        if "client_max_window_bits" in websocket_deflate_parameters:
            window_bits = int(websocket_deflate_parameters["client_max_window_bits"])
        self.websocket_deflate_decompressor[stream_id][Direction.CLIENTTOSERVER] = (
            zlib.decompressobj(wbits=-window_bits)
        )

    def decode_websocket_extensions(
        self, extensions_header: str
    ) -> Dict[str, Dict[str, Union[bool, str]]]:
        extensions: Dict[str, Dict[str, Union[str, bool]]] = {}
        if extensions_header:
            raw_extensions = map(
                lambda s: s.strip().lower(), extensions_header.split(",")
            )
            for extension in raw_extensions:
                extension, raw_params = (
                    extension.split(";", 1) if ";" in extension else (extension, "")
                )
                params: Dict[str, Union[str, bool]] = {}
                reduced_raw_params = filter(
                    lambda p: len(p) != 0,
                    map(lambda p: p.strip(), raw_params.split(";")),
                )
                for param in reduced_raw_params:
                    param_and_value = param.split("=", 1)
                    if len(param_and_value) == 1:
                        params[param_and_value[0]] = True
                    else:
                        if param_and_value[1].startswith('"') and param_and_value[1].endswith('"'):
                            param_and_value[1] = param_and_value[1][1:-1]
                        params[param_and_value[0]] = param_and_value[1]
                extensions[extension] = params
        return extensions

    def handle_http2_headers(
        self,
        direction: Direction,
        frame: hyperframe.frame.Frame,
        headers: List[HeaderTuple],
    ) -> None:
        if (
            not self.websocket_enable_connect_protocol
            or direction != Direction.CLIENTTOSERVER
        ):
            return

        # The client wants to use the extended CONNECT protocol
        # using the ":protocol" pseudo header.
        protocol = None
        scheme = None
        extensions_header = None
        for header in headers:
            if header[0] == ":protocol":
                protocol = header[1].decode()
            elif header[0] == ":scheme":
                scheme = header[1].decode()
            elif header[0] == "sec-websocket-extensions":
                extensions_header = header[1].decode()

        # The client doesn't want to use the extended CONNECT protocol
        if protocol is None:
            return

        if protocol != "websocket":
            return

        # I doubt we'll see "https" yet, but let's be prepared
        if scheme not in ["http", "https"]:
            return

        # Decode extensions header
        if extensions_header is not None:
            # sec-websocket-extensions: extension-name; param1=value1; param2="value2", extension-name2; param1, extension-name3, extension-name4; param1=value1
            extensions = self.decode_websocket_extensions(extensions_header)
            if "permessage-deflate" in extensions:
                self.handle_permessage_deflate_extension(
                    frame.stream_id, extensions["permessage-deflate"]
                )
            elif len(extensions) > 0:
                self.log(f"Unsupported extensions: {extensions}")

        # Switch this stream to websocket mode
        self.websocket_stream[frame.stream_id] = True

    def handle_http2_event(
        self, direction: Direction, frame: hyperframe.frame.Frame
    ) -> bytes:
        if isinstance(frame, hyperframe.frame.SettingsFrame):
            if (
                direction != Direction.SERVERTOCLIENT
                or frame.settings.get(frame.ENABLE_CONNECT_PROTOCOL, 0) == 0
            ):
                return super().handle_http2_event(direction, frame)

            # The client can use the extended CONNECT protocol
            # using the ":protocol" pseudo header.
            self.websocket_enable_connect_protocol = True
        elif isinstance(frame, hyperframe.frame.DataFrame):
            if not self.websocket_stream[frame.stream_id]:
                return super().handle_http2_event(direction, frame)

            return self.handle_websocket_frames(direction, frame.stream_id, frame.data)

        return super().handle_http2_event(direction, frame)

    def handle_raw_client_chunk(
        self, chunk: StreamChunk
    ) -> Optional[List[StreamChunk]]:
        try:
            if self.switched_protocols:
                return [
                    chunk.derive(
                        content=self.handle_websocket_frames(
                            chunk.Direction, 0, chunk.Content
                        ),
                    )
                ]
            return super().handle_raw_client_chunk(chunk)
        except Exception as ex:
            return [chunk.derive(content=str(ex).encode())]

    def handle_raw_server_chunk(
        self, chunk: StreamChunk
    ) -> Optional[List[StreamChunk]]:
        try:
            if self.switched_protocols:
                return [
                    chunk.derive(
                        content=self.handle_websocket_frames(
                            chunk.Direction, 0, chunk.Content
                        ),
                    )
                ]
            return super().handle_raw_server_chunk(chunk)
        except Exception as ex:
            return [chunk.derive(content=str(ex).encode())]

    def handle_http1_request(
        self, chunk: StreamChunk, request: HTTPRequest
    ) -> List[StreamChunk]:
        # Allow "Connection: keep-alive, Upgrade"
        connection = (
            request.headers.get("Connection", "").lower().replace(" ", "").split(",")
        )
        if (
            "upgrade" in connection
            and request.headers.get("Upgrade", "").lower() == "websocket"
        ):
            websocket_key = request.headers.get("Sec-WebSocket-Key", None)
            if websocket_key is None:
                return [chunk.derive(content=b"No websocket key found")]
            self.websocket_key = websocket_key.encode()

        return super().handle_http1_request(chunk, request)

    def handle_http1_response(
        self, header: bytes, body: bytes, chunk: StreamChunk, response: HTTPResponse
    ) -> List[StreamChunk]:
        try:
            connection = (
                response.headers.get("Connection", "")
                .lower()
                .replace(" ", "")
                .split(",")
            )
            if (
                response.status == 101
                and "upgrade" in connection
                and response.headers.get("Upgrade", "").lower() == "websocket"
            ):
                if not self.websocket_key:
                    raise Exception("No websocket key found")
                expected_accept = b64encode(
                    sha1(
                        self.websocket_key + b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
                    ).digest()
                ).decode()
                if response.headers.get("Sec-WebSocket-Accept") != expected_accept:
                    raise Exception(
                        f"Invalid websocket key: {response.headers.get('Sec-WebSocket-Accept')} != {expected_accept}"
                    )
                self.switched_protocols = True

                # Decode extensions header
                # Sec-WebSocket-Extensions: extension-name; param1=value1; param2="value2", extension-name2; param1, extension-name3, extension-name4; param1=value1
                extensions_header = response.headers.get("Sec-WebSocket-Extensions")
                extensions = self.decode_websocket_extensions(extensions_header)
                if "permessage-deflate" in extensions:
                    self.handle_permessage_deflate_extension(
                        0, extensions["permessage-deflate"]
                    )
                elif len(extensions) > 0:
                    self.log(f"Unsupported extensions: {extensions}")

                data = b""
                if len(body) > 0:
                    data = self.handle_websocket_frames(chunk.Direction, 0, body)

                return [chunk.derive(content=header + b"\r\n\r\n" + data)]

            return super().handle_http1_response(header, body, chunk, response)
        except Exception as ex:
            return [
                chunk.derive(
                    content=f"Unable to parse HTTP1 response (websockets): {ex}".encode(),
                )
            ]

    def handle_stream(self, stream: Stream) -> Result:
        # http1
        self.websocket_key = None
        self.switched_protocols = False

        # http2
        self.websocket_enable_connect_protocol = False
        self.websocket_stream = defaultdict(bool)

        # deflate extension
        self.websocket_deflate = defaultdict(bool)
        self.websocket_deflate_decompressor = defaultdict(dict)
        self.websocket_message_fragmented_frames = defaultdict(list)

        self.websocket_remaining_data = defaultdict(lambda: defaultdict(bytes))
        return super().handle_stream(stream)


if __name__ == "__main__":
    WebsocketConverter().run()
