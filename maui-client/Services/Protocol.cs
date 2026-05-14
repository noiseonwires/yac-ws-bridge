using System.Buffers.Binary;
using System.Text;

namespace BridgeToFreedom.Services;

/// <summary>
/// Wire protocol for the IP-tunnel build, mirroring the Go adapter/helper.
/// Format: [1B type][payload...]
/// PACKET payload = one raw IP packet.
/// PACKET_BATCH payload = repeated [2B lenBE][rawIPpacket].
/// </summary>
public static class Protocol
{
    // Control
    public const byte MsgHello       = 0x01;
    public const byte MsgHelloOK     = 0x02;
    public const byte MsgHelloErr    = 0x03;
    public const byte MsgPeerConn    = 0x04;
    public const byte MsgPeerGone    = 0x05;
    public const byte MsgSync        = 0x06;
    public const byte MsgPing        = 0xF0;
    public const byte MsgPong        = 0xF1;

    // Data
    public const byte MsgPacket      = 0x10;
    public const byte MsgPacketBatch = 0x11;

    public static string MsgName(byte type) => type switch
    {
        MsgHello       => "HELLO",
        MsgHelloOK     => "HELLO_OK",
        MsgHelloErr    => "HELLO_ERR",
        MsgPeerConn    => "PEER_CONN",
        MsgPeerGone    => "PEER_GONE",
        MsgSync        => "SYNC",
        MsgPing        => "PING",
        MsgPong        => "PONG",
        MsgPacket      => "PACKET",
        MsgPacketBatch => "PACKET_BATCH",
        _ => $"0x{type:X2}"
    };

    public static byte[] Encode(byte type, byte[]? payload = null)
    {
        var p = payload ?? [];
        var buf = new byte[1 + p.Length];
        buf[0] = type;
        if (p.Length > 0) p.CopyTo(buf, 1);
        return buf;
    }

    public static (byte Type, byte[] Payload) Decode(byte[] data)
    {
        if (data.Length < 1) throw new InvalidDataException("frame too short");
        var payload = data.Length > 1 ? data[1..] : [];
        return (data[0], payload);
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

    /// <summary>
    /// Decodes a PACKET_BATCH payload, invoking <paramref name="onPacket"/> for each
    /// embedded IP packet. The slice handed to the callback is owned by <paramref name="payload"/>;
    /// copy if you need to retain it.
    /// </summary>
    public static int DecodePacketBatch(byte[] payload, Action<ArraySegment<byte>> onPacket)
    {
        int off = 0;
        int count = 0;
        while (off < payload.Length)
        {
            if (off + 2 > payload.Length)
                throw new InvalidDataException("PACKET_BATCH: truncated length prefix");
            var len = BinaryPrimitives.ReadUInt16BigEndian(payload.AsSpan(off));
            off += 2;
            if (off + len > payload.Length)
                throw new InvalidDataException("PACKET_BATCH: truncated packet");
            onPacket(new ArraySegment<byte>(payload, off, len));
            off += len;
            count++;
        }
        return count;
    }

    public static byte[] EncodePacketBatch(IReadOnlyList<byte[]> packets)
    {
        int total = 0;
        for (int i = 0; i < packets.Count; i++) total += 2 + packets[i].Length;
        var buf = new byte[total];
        int off = 0;
        for (int i = 0; i < packets.Count; i++)
        {
            var p = packets[i];
            BinaryPrimitives.WriteUInt16BigEndian(buf.AsSpan(off), (ushort)p.Length);
            off += 2;
            p.CopyTo(buf, off);
            off += p.Length;
        }
        return buf;
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
