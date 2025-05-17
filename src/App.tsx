// src/App.tsx
import React, { useEffect, useState } from 'react'
import 'leaflet/dist/leaflet.css' // ← ensure Leaflet’s CSS is loaded
import { MapContainer, TileLayer, useMap, Marker, Tooltip, useMapEvent } from 'react-leaflet'
import type { LatLngExpression } from 'leaflet'
import ReconnectingWebSocket from 'reconnecting-websocket'

// shape of messages we send/receive
interface CursorMessage {
  userUUID: string
  projectUUID: string
  lat: number
  lng: number
  timestamp: number
}

interface OtherCursor {
  lat: number
  lng: number
  lastSeen: number
}

const WS_URL = 'ws://localhost:8989/ws'

// Component that captures mouse moves and WS messages
const CursorTracker: React.FC<{
  ws: ReconnectingWebSocket
  userUUID: string
  projectUUID: string
  updateCursors: (msg: CursorMessage) => void
}> = ({ ws, userUUID, projectUUID, updateCursors }) => {
  useMapEvent('mousemove', (e) => {
    const msg: CursorMessage = {
      userUUID,
      projectUUID,
      lat: e.latlng.lat,
      lng: e.latlng.lng,
      timestamp: Date.now(),
    }
    ws.send(JSON.stringify(msg))
  })

  useEffect(() => {
    const handler = (event: MessageEvent) => {
      try {
        const data: CursorMessage = JSON.parse(event.data)
        if (data.projectUUID === projectUUID && data.userUUID !== userUUID) {
          updateCursors(data)
        }
      } catch {
        console.warn('Received invalid WS message')
      }
    }
    ws.addEventListener('message', handler)
    return () => {
      ws.removeEventListener('message', handler)
    }
  }, [ws, projectUUID, userUUID, updateCursors])

  return null
}

// Helper component to invalidate map size on mount
const ResizeHandler: React.FC = () => {
  const map = useMap()
  useEffect(() => {
    map.invalidateSize() // ← force a resize check so all tiles load
  }, [map])
  return null
}

const App: React.FC = () => {
  const [ws, setWs] = useState<ReconnectingWebSocket | null>(null)
  const [cursors, setCursors] = useState<Record<string, OtherCursor>>({})

  const userUUID = 'user-123'
  const projectUUID = 'project-abc'

  // Initialize WS once
  useEffect(() => {
    const socket = new ReconnectingWebSocket(`${WS_URL}?user_uuid=${userUUID}&project_uuid=${projectUUID}`)
    setWs(socket)
    return () => {
      socket.close()
    }
  }, [userUUID, projectUUID])

  // Add/update remote cursors
  const updateCursors = (msg: CursorMessage) => {
    setCursors((prev) => ({
      ...prev,
      [msg.userUUID]: { lat: msg.lat, lng: msg.lng, lastSeen: msg.timestamp },
    }))
  }

  // Prune stale cursors every 5s
  useEffect(() => {
    const interval = setInterval(() => {
      const cutoff = Date.now() - 15000
      setCursors((prev) => {
        const kept: Record<string, OtherCursor> = {}
        for (const [uid, cur] of Object.entries(prev)) {
          if (cur.lastSeen > cutoff) kept[uid] = cur
        }
        return kept
      })
    }, 5000)
    return () => clearInterval(interval)
  }, [])

  const center: LatLngExpression = [51.505, -0.09]

  return (
    <div className="h-screen w-screen">
      <MapContainer center={center} zoom={13} className="h-full w-full">
        <TileLayer
          attribution="&copy; OpenStreetMap contributors"
          url="https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png"
        />
        <ResizeHandler /> {/* ensure tiles fetch correctly */}
        {ws && <CursorTracker ws={ws} userUUID={userUUID} projectUUID={projectUUID} updateCursors={updateCursors} />}
        {ws &&
          Object.entries(cursors).map(([uid, cur]) => (
            <Marker key={uid} position={[cur.lat, cur.lng]}>
              <Tooltip direction="top" offset={[0, -10]} opacity={1} permanent>
                {uid}
              </Tooltip>
            </Marker>
          ))}
      </MapContainer>
    </div>
  )
}

export default App
