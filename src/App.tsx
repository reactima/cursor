// src/App.tsx
import React, { useEffect, useState } from 'react'
import { MapContainer, TileLayer, useMapEvent, Marker, Tooltip } from 'react-leaflet'
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

// Your WS endpoint (adjust host/port as needed)
const WS_URL = 'ws://localhost:8989/ws'

// Component that attaches to the map, listens for mouse moves, sends & receives WS messages
const CursorTracker: React.FC<{
  ws: ReconnectingWebSocket
  userUUID: string
  projectUUID: string
  updateCursors: (msg: CursorMessage) => void
}> = ({ ws, userUUID, projectUUID, updateCursors }) => {
  // Capture mousemove events on map and send to server
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

  // Listen for incoming messages and update shared state
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

const App: React.FC = () => {
  const [ws, setWs] = useState<ReconnectingWebSocket | null>(null)
  const [cursors, setCursors] = useState<Record<string, OtherCursor>>({})

  // Replace these with real UUIDs in your app
  const userUUID = 'user-123'
  const projectUUID = 'project-abc'

  // Initialize WS once on mount
  useEffect(() => {
    const socket = new ReconnectingWebSocket(`${WS_URL}?user_uuid=${userUUID}&project_uuid=${projectUUID}`)
    setWs(socket)
    return () => {
      socket.close()
    }
  }, [userUUID, projectUUID])

  // Add or update a remote cursor in state
  const updateCursors = (msg: CursorMessage) => {
    setCursors((prev) => ({
      ...prev,
      [msg.userUUID]: { lat: msg.lat, lng: msg.lng, lastSeen: msg.timestamp },
    }))
  }

  // Periodically prune cursors that haven’t updated in 15s
  useEffect(() => {
    const interval = setInterval(() => {
      const cutoff = Date.now() - 15000
      setCursors((prev) => {
        const filtered: Record<string, OtherCursor> = {}
        for (const [uid, cur] of Object.entries(prev)) {
          if (cur.lastSeen > cutoff) filtered[uid] = cur
        }
        return filtered
      })
    }, 5000)
    return () => clearInterval(interval)
  }, [])

  // Default center of the map
  const center: LatLngExpression = [51.505, -0.09]

  return (
    <div className="h-screen w-screen">
      {ws && (
        <MapContainer center={center} zoom={13} className="h-full w-full">
          <TileLayer
            attribution="&copy; OpenStreetMap contributors"
            url="https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png"
          />
          <CursorTracker ws={ws} userUUID={userUUID} projectUUID={projectUUID} updateCursors={updateCursors} />
          {Object.entries(cursors).map(([uid, cur]) => (
            <Marker key={uid} position={[cur.lat, cur.lng]}>
              <Tooltip direction="top" offset={[0, -10]} opacity={1} permanent>
                {uid}
              </Tooltip>
            </Marker>
          ))}
        </MapContainer>
      )}
    </div>
  )
}

export default App
