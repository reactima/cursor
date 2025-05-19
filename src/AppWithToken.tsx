import React, { useCallback, useEffect, useRef, useState, useMemo } from 'react'
import 'leaflet/dist/leaflet.css'
import { MapContainer, TileLayer, useMap, Marker, Tooltip } from 'react-leaflet'
import type { LatLngExpression } from 'leaflet'
import L from 'leaflet'
import ReconnectingWebSocket from 'reconnecting-websocket'
import { notification, Input, Button } from 'antd'
import { v4 as uuidv4 } from 'uuid'
import { createClient } from '@supabase/supabase-js'

// leaflet icon fix for bundlers
import markerIcon2x from 'leaflet/dist/images/marker-icon-2x.png'
import markerIcon from 'leaflet/dist/images/marker-icon.png'
import markerShadow from 'leaflet/dist/images/marker-shadow.png'
L.Icon.Default.mergeOptions({ iconRetinaUrl: markerIcon2x, iconUrl: markerIcon, shadowUrl: markerShadow })

// Supabase client
const supabase = createClient(process.env.REACT_APP_SUPABASE_URL!, process.env.REACT_APP_SUPABASE_ANON_KEY!)

// utility
const genUsername = () => {
  const adj = ['Swift', 'Bright', 'Silent', 'Mighty', 'Clever', 'Happy', 'Brave', 'Lucky', 'Cosmic', 'Fuzzy']
  const noun = ['Tiger', 'Eagle', 'Lion', 'Wolf', 'Falcon', 'Panda', 'Dolphin', 'Dragon', 'Otter', 'Hawk']
  return `${adj[Math.floor(Math.random() * adj.length)]}${noun[Math.floor(Math.random() * noun.length)]}${Math.floor(
    Math.random() * 100
  )}`
}

// message types
interface CursorMessage {
  userUUID: string
  username: string
  projectUUID: string
  lat: number
  lng: number
  timestamp: number
}
interface OtherCursor {
  username: string
  lat: number
  lng: number
  lastSeen: number
}

const WS_URL = window.location.hostname === 'localhost' ? 'ws://localhost:8989/ws' : 'wss://cursor.reactima.com/ws'

/* ---------- hook for sending/receiving cursor data ---------- */
const useCursorTracker = (
  ws: ReconnectingWebSocket | null,
  userUUID: string,
  username: string,
  projectUUID: string,
  report: (msg: CursorMessage) => void
) => {
  const map = useMap()

  useEffect(() => {
    if (!ws) return
    const onMouse = (e: L.LeafletMouseEvent) => {
      const msg: CursorMessage = {
        userUUID,
        username,
        projectUUID,
        lat: e.latlng.lat,
        lng: e.latlng.lng,
        timestamp: Date.now(),
      }
      report(msg)
      ws.send(JSON.stringify(msg))
    }
    map.on('mousemove', onMouse)
    return () => map.off('mousemove', onMouse)
  }, [ws, map, userUUID, username, projectUUID, report])

  useEffect(() => {
    if (!ws) return
    const onMsg = (ev: MessageEvent) => {
      try {
        const data: CursorMessage = JSON.parse(ev.data)
        if (data.projectUUID === projectUUID) report(data)
      } catch {
        // ignore
      }
    }
    ws.addEventListener('message', onMsg)
    return () => ws.removeEventListener('message', onMsg)
  }, [ws, projectUUID, report])
}

const CursorTracker: React.FC<{
  ws: ReconnectingWebSocket
  userUUID: string
  username: string
  projectUUID: string
  report: (msg: CursorMessage) => void
}> = (p) => {
  useCursorTracker(p.ws, p.userUUID, p.username, p.projectUUID, p.report)
  return null
}

const ResizeHandler: React.FC = () => {
  const map = useMap()
  useEffect(() => {
    map.invalidateSize()
  }, [map])
  return null
}

const App: React.FC = () => {
  const [username, setUsername] = useState(genUsername())
  const [projectUUID, setProjectUUID] = useState(uuidv4())
  const [userUUID, setUserUUID] = useState(uuidv4())
  const [ws, setWs] = useState<ReconnectingWebSocket | null>(null)
  const [connected, setConnected] = useState(false)
  const [cursors, setCursors] = useState<Record<string, OtherCursor>>({})
  const mapRef = useRef<L.Map | null>(null)

  const updateCursors = useCallback((msg: CursorMessage) => {
    setCursors((prev) => ({
      ...prev,
      [msg.userUUID]: { username: msg.username, lat: msg.lat, lng: msg.lng, lastSeen: msg.timestamp },
    }))
  }, [])

  useEffect(() => {
    const id = setInterval(() => {
      const cutoff = Date.now() - 15000
      setCursors((prev) => {
        const kept: Record<string, OtherCursor> = {}
        for (const [uid, cur] of Object.entries(prev)) if (cur.lastSeen > cutoff) kept[uid] = cur
        return kept
      })
    }, 5000)
    return () => clearInterval(id)
  }, [])

  useEffect(() => {
    if (!connected || !ws || !mapRef.current) return
    const pos = mapRef.current.getCenter()
    const hello: CursorMessage = {
      userUUID,
      username,
      projectUUID,
      lat: pos.lat,
      lng: pos.lng,
      timestamp: Date.now(),
    }
    updateCursors(hello)
    ws.send(JSON.stringify(hello))
  }, [connected, ws, mapRef, userUUID, username, projectUUID, updateCursors])

  const toggle = async () => {
    if (connected) {
      ws?.close()
      setWs(null)
      setConnected(false)
      setCursors({})
      return
    }
    const newUID = uuidv4()
    setUserUUID(newUID)
    setCursors({})

    // get Supabase JWT from session
    const {
      data: { session },
      error,
    } = await supabase.auth.getSession()
    if (error || !session) {
      notification.error({ message: 'Auth error', description: error?.message || 'No session' })
      return
    }
    const token = session.access_token

    // open WebSocket with Bearer token as subprotocol
    const protocols = [`Bearer ${token}`]
    const socket = new ReconnectingWebSocket(`${WS_URL}?user_uuid=${newUID}&project_uuid=${projectUUID}`, protocols)

    socket.addEventListener('open', () => {
      notification.success({
        message: 'Connected',
        description: `Project ${projectUUID} – you are ${username}`,
        placement: 'bottomRight',
        duration: 3,
      })
      setConnected(true)
    })
    socket.addEventListener('close', () => setConnected(false))
    setWs(socket)
  }

  const users = useMemo(() => {
    const list = Object.values(cursors)
      .sort((a, b) => a.username.localeCompare(b.username))
      .map((c) => c.username)
    if (!list.includes(username)) list.unshift(username)
    return list
  }, [cursors, username])

  const center: LatLngExpression = [51.505, -0.09]

  return (
    <div className="h-screen w-screen relative">
      <div className="absolute top-2 left-[50px] z-[1000] bg-white/90 backdrop-blur rounded shadow p-2 flex gap-3 items-start">
        <div className="flex flex-col gap-2">
          <Input
            size="small"
            value={username}
            disabled={connected}
            onChange={(e) => setUsername(e.target.value)}
            placeholder="Username"
            style={{ width: 130 }}
          />
          <Input
            size="small"
            value={projectUUID}
            disabled={connected}
            onChange={(e) => setProjectUUID(e.target.value)}
            placeholder="Project UUID"
            style={{ width: 220 }}
          />
          <Button size="small" type="primary" onClick={toggle}>
            {connected ? 'Disconnect' : 'Connect'}
          </Button>
        </div>
        <div className="text-xs space-y-1">
          <div className="font-semibold">Users ({users.length})</div>
          <ul className="list-disc list-inside space-y-px max-h-40 overflow-y-auto pr-1">
            {users.map((u) => (
              <li key={u}>{u}</li>
            ))}
          </ul>
        </div>
      </div>

      <MapContainer
        center={center}
        zoom={13}
        className="h-full w-full"
        whenCreated={(m) => {
          mapRef.current = m
        }}
      >
        <TileLayer
          attribution="© OpenStreetMap contributors"
          url="https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png"
        />
        <ResizeHandler />
        {ws && (
          <CursorTracker
            ws={ws}
            userUUID={userUUID}
            username={username}
            projectUUID={projectUUID}
            report={updateCursors}
          />
        )}
        {Object.entries(cursors).map(([uid, cur]) => (
          <Marker key={uid} position={[cur.lat, cur.lng]}>
            <Tooltip direction="top" offset={[0, -10]} opacity={1} permanent>
              {cur.username}
            </Tooltip>
          </Marker>
        ))}
      </MapContainer>
    </div>
  )
}

export default App
