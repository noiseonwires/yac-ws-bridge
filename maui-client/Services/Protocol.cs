using System.Buffers.Binary;
using System.Text;

namespace BridgeToFreedom.Services;

/// <summary>
/// Binary protocol matching the Go adapter/helper protocol exactly.
/// Wire format: [1B type][4B streamID BE][payload...]
/// </summary>
public static class Protocol
{
    // Control messages (streamID = 0)
    public const byte MsgHello    = 0x01;
    public const byte MsgHelloOK  = 0x02;
    public const byte MsgHelloErr = 0x03;
    public const byte MsgPeerConn = 0x04;
    public const byte MsgPeerGone = 0x05;
    public const byte MsgSync     = 0x06;
    public const byte MsgPing     = 0xF0;
    public const byte MsgPong     = 0xF1;

    // Stream messages (streamID > 0)
    public const byte MsgOpen     = 0x10;
    public const byte MsgOpenOK   = 0x11;
    public const byte MsgOpenFail = 0x12;
    public const byte MsgData     = 0x20;
    public const byte MsgFin      = 0x21;
    public const byte MsgRst      = 0x22;

    public static string MsgName(byte type) => type switch
    {
        MsgHello    => "HELLO",
        MsgHelloOK  => "HELLO_OK",
        MsgHelloErr => "HELLO_ERR",
        MsgPeerConn => "PEER_CONN",
        MsgPeerGone => "PEER_GONE",
        MsgSync     => "SYNC",
        MsgPing     => "PING",
        MsgPong     => "PONG",
        MsgOpen     => "OPEN",
        MsgOpenOK   => "OPEN_OK",
        MsgOpenFail => "OPEN_FAIL",
        MsgData     => "DATA",
        MsgFin      => "FIN",
        MsgRst      => "RST",
        _ => $"0x{type:X2}"
    };

    public static byte[] Encode(byte type, uint streamId, byte[]? payload = null)
    {
        var p = payload ?? [];
        var buf = new byte[5 + p.Length];
        buf[0] = type;
        BinaryPrimitives.WriteUInt32BigEndian(buf.AsSpan(1), streamId);
        if (p.Length > 0) p.CopyTo(buf, 5);
        return buf;
    }

    public static (byte Type, uint StreamId, byte[] Payload) Decode(byte[] data)
    {
        if (data.Length < 5) throw new InvalidDataException("frame too short");
        var type = data[0];
        var streamId = BinaryPrimitives.ReadUInt32BigEndian(data.AsSpan(1));
        var payload = data.Length > 5 ? data[5..] : [];
        return (type, streamId, payload);
    }

    public static byte[] EncodeHello(byte version, string token)
    {
        var t = Encoding.UTF8.GetBytes(token);
        var buf = new byte[1 + t.Length];
        buf[0] = version;
        t.CopyTo(buf, 1);
        return buf;
    }

    public static (string OwnId, string PeerId, string IamToken) DecodeHelloOK(byte[] payload)
    {
        int off = 0;
        var ownId = ReadLenPrefixed(payload, ref off);
        var peerId = ReadLenPrefixed(payload, ref off);
        var iamToken = ReadLenPrefixed(payload, ref off);
        return (ownId, peerId, iamToken);
    }

    public static (string PeerId, string IamToken) DecodePeerConn(byte[] payload)
    {
        int off = 0;
        var peerId = ReadLenPrefixed(payload, ref off);
        var iamToken = ReadLenPrefixed(payload, ref off);
        return (peerId, iamToken);
    }

    public static string DecodePong(byte[] payload)
    {
        int off = 0;
        return ReadLenPrefixed(payload, ref off);
    }

    private static string ReadLenPrefixed(byte[] data, ref int off)
    {
        if (off + 2 > data.Length) throw new InvalidDataException("payload too short");
        var len = BinaryPrimitives.ReadUInt16BigEndian(data.AsSpan(off));
        off += 2;
        if (off + len > data.Length) throw new InvalidDataException("payload truncated");
        var s = Encoding.UTF8.GetString(data, off, len);
        off += len;
        return s;
    }
}
