# Logging y observabilidad

## Auditoría inicial

renfecli es un CLI Go: `cmd/renfe` despacha comandos; `internal/client` hace HTTP contra Renfe; `internal/config` lee TOML y `internal/store` persiste catálogo/sesión. No hay servidor, base de datos, colas ni servicios en segundo plano salvo `session keep`. Los datos del comando van a stdout y los diagnósticos a stderr. Antes de este cambio ya existía un callback opcional del cliente para reintentos y fallback TLS, más avisos de CLI en texto libre; faltaban niveles, formato configurable, eventos estructurados y criterios centralizados. Los errores HTTP incluían hasta 300 caracteres de respuesta remota.

Puntuación inicial (0–10): arquitectura 4, cobertura 3, utilidad 4, estructura 1, correlación 1, seguridad 4, resiliencia 3, configuración 2, errores 4, pruebas 3. Media: **2,9/10**. El CLI no necesita métricas/tracing distribuidos: una invocación es un proceso local y no hay servidor ni llamadas entre servicios.

## Implementación

La CLI usa `log/slog` de la biblioteca estándar y escribe solo en stderr. Los eventos tienen `service=renfecli`; el texto legible es el formato por defecto y JSON se activa con `RENFE_LOG_FORMAT=json`. `RENFE_LOG_LEVEL` admite `debug`, `info` (predeterminado), `warn` y `error`; valores desconocidos usan `info`. TRACE y FATAL no forman parte de los niveles estándar de `slog` y no se necesitan para estos flujos.

El límite de despacho registra `command started` y un evento de finalización o fallo con el nombre del comando, resultado y `duration_ms`. Solo registra el nombre de operación; nunca argumentos, que pueden contener información de viaje o cuenta. Los comandos de ayuda/versión también quedan cubiertos.

Las operaciones semánticas de almacenamiento registran lecturas fallidas de sesiones y catálogos, el guardado de una sesión sin identidad ni cookie, y la caché de estaciones con hit/refresco/fallback, conteo, antigüedad y duración. Un archivo ausente es normal; corrupción/permisos generan WARN. No se instrumenta la biblioteca `store` genérica para evitar estado global y mantenerla libre de dependencia del proceso.

El cliente no depende del logger: `LogEvent` es opcional y el proceso lo conecta a `slog`; `Logf` sigue disponible como fallback para embedders existentes. Cada intercambio HTTP emite en DEBUG método, ruta sin query, estado/resultado y duración; los errores de transporte/lectura registran solo su tipo. Los reintentos son WARN con `status`, `operation`, `attempt`, `max_attempts` y `retry_after_ms`; el fallback TLS es WARN con tipo de error, sin volcar el mensaje del transporte. Errores HTTP exponen el estado y mantienen el cuerpo solo en el objeto para diagnóstico local; `Error()` no vuelca datos remotos potencialmente privados.

No registrar cookies, cabeceras, payloads, itinerarios ni cuerpos HTTP. Mensajes que contengan datos externos deben pasar por `safeText`; campos de una línea por `safeField`. No incluir secretos como atributos estructurados. Los errores operativos se presentan una vez en el límite CLI; fallos recuperables se emiten donde se decide la recuperación.

Ejemplo JSON:

```json
{"time":"2026-11-11T12:00:00Z","level":"INFO","msg":"throttled: HTTP 429 on /vol/... — retrying 1/3 in 500ms","service":"renfecli"}
```

Para diagnosticar: enviar stderr a la herramienta del sistema; usar `RENFE_LOG_FORMAT=json` para ingestión y `RENFE_LOG_LEVEL=debug` cuando existan eventos de diagnóstico de ese nivel. No hay rotación de archivos ni destino remoto: los opera el entorno que lanza el proceso.

## Limitaciones conocidas

Además del ciclo de vida de comandos, la cobertura incluye resultados HTTP en DEBUG, fallos de recuperación, persistencia de sesión y caché de estaciones. No hay correlación/tracing ni métricas; una única ejecución local no justifica infraestructura distribuida. No hay buffering asíncrono ni rotación local; stdout/stderr quedan bajo control del proceso supervisor. Algunos diagnósticos aún son mensajes heredados, así que el esquema operacional no está completo. Por ello no se reclama 10/10.

## Revisión intermedia

Tras añadir eventos de ciclo de vida, eventos HTTP DEBUG, persistencia/caché, niveles WARN y pruebas de serialización/redacción: arquitectura 6, cobertura 8, utilidad 7, estructura 8, correlación 2, seguridad 8, rendimiento 6, configuración 7, errores 8, pruebas 9. Media **6,9/10**. Es una valoración intermedia, no una puntuación final; falta correlación, cobertura de todos los fallos de almacenamiento, concurrencia de logging y más pruebas de privacidad.
