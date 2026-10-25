// 3D preview for the card: loads a GLB (or FBX) that arrives inline in the
// tool result, frames it, and offers textured / clay / parts views. No
// WebAssembly: the server decodes meshopt before it serves the model.
import * as THREE from "three";
import { GLTFLoader } from "three/addons/loaders/GLTFLoader.js";
import { FBXLoader } from "three/addons/loaders/FBXLoader.js";
import { OrbitControls } from "three/addons/controls/OrbitControls.js";
import { RoomEnvironment } from "three/addons/environments/RoomEnvironment.js";

const PART_COLORS = (n) =>
  Array.from({ length: n }, (_, i) => new THREE.Color().setHSL((i * 0.618034 + 0.04) % 1, 0.62, 0.58));

export class Viewer {
  constructor(stage) {
    this.meshes = [];
    this.mode = "textured";
    this.wire = false;

    const renderer = (this.renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true }));
    renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
    renderer.outputColorSpace = THREE.SRGBColorSpace;
    // Neutral (Khronos PBR) tone mapping keeps the texture's own colours;
    // ACES washed out light tones.
    renderer.toneMapping = THREE.NeutralToneMapping;
    renderer.toneMappingExposure = 1.0;

    const scene = (this.scene = new THREE.Scene());
    const pmrem = new THREE.PMREMGenerator(renderer);
    scene.environment = pmrem.fromScene(new RoomEnvironment(), 0.04).texture;
    const key = new THREE.DirectionalLight(0xfff4e8, 1.15);
    key.position.set(3, 5, 4);
    const rim = new THREE.DirectionalLight(0xffb48a, 0.9); // warm back light lifts the silhouette off the dark stage
    rim.position.set(-4, 3, -5);
    scene.add(key, rim);

    this.camera = new THREE.PerspectiveCamera(32, 1, 0.01, 1000);
    const controls = (this.controls = new OrbitControls(this.camera, renderer.domElement));
    controls.enableDamping = true;
    controls.dampingFactor = 0.08;
    controls.enableZoom = false; // the chat scrolls; ctrl/cmd + wheel zooms (below)
    controls.autoRotate = true;
    controls.autoRotateSpeed = 1.1;
    controls.addEventListener("start", () => (controls.autoRotate = false));
    renderer.domElement.addEventListener(
      "wheel",
      (e) => {
        if (!e.ctrlKey && !e.metaKey) return;
        e.preventDefault();
        const offset = this.camera.position.clone().sub(controls.target);
        offset.multiplyScalar(Math.exp(Math.sign(e.deltaY) * 0.12));
        const len = THREE.MathUtils.clamp(offset.length(), this.radius * 0.6, this.radius * 12);
        this.camera.position.copy(controls.target).add(offset.setLength(len));
        controls.autoRotate = false;
      },
      { passive: false }
    );

    this.visible = true;
    this.attach(stage);
    const tick = () => {
      requestAnimationFrame(tick);
      if (!this.visible || !this.model) return;
      this.stepExplode();
      controls.update();
      renderer.render(scene, this.camera);
    };
    tick();
  }

  // attach moves the canvas into a stage element; the card re-renders its
  // DOM, but the WebGL context lives on.
  attach(stage) {
    this.stage = stage;
    stage.prepend(this.renderer.domElement);
    this.ro?.disconnect();
    this.io?.disconnect();
    (this.ro = new ResizeObserver(() => this.resize())).observe(stage);
    (this.io = new IntersectionObserver(([e]) => (this.visible = e.isIntersecting))).observe(stage);
  }

  resize() {
    const { clientWidth: w, clientHeight: h } = this.stage;
    if (!w || !h) return;
    this.renderer.setSize(w, h, false);
    this.camera.aspect = w / h;
    this.camera.updateProjectionMatrix();
  }

  // model: { data: base64, format: "glb" | "fbx" } straight from the tool result.
  async load(model) {
    if (model.error) throw new Error(model.error);
    const bin = atob(model.data);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    const buf = bytes.buffer;
    let root;
    if (model.format === "glb") root = (await new GLTFLoader().parseAsync(buf, "")).scene;
    else if (model.format === "fbx") root = new FBXLoader().parse(buf, "");
    else throw new Error("not a GLB or FBX model");
    this.setModel(root);
  }

  setModel(root) {
    this.clear();
    const aniso = this.renderer.capabilities.getMaxAnisotropy();
    root.traverse((o) => {
      if (!o.isMesh) return;
      o.userData.material = o.material;
      // Sharp textures at grazing angles and when zoomed in.
      for (const m of [].concat(o.material)) for (const v of Object.values(m || {})) if (v?.isTexture) v.anisotropy = aniso;
      this.meshes.push(o);
    });
    // Center on the floor, then frame.
    const box = new THREE.Box3().setFromObject(root);
    const center = box.getCenter(new THREE.Vector3());
    root.position.sub(new THREE.Vector3(center.x, box.min.y, center.z));
    const size = box.getSize(new THREE.Vector3());
    this.radius = Math.max(size.length() / 2, 1e-3);
    this.scene.add(root);
    this.model = root;
    this.addShadow(size);
    this.home = { target: new THREE.Vector3(0, size.y * 0.5, 0) };
    this.prepareExplode();
    if (this.mode !== "textured" || this.wire) this.setMode(this.mode); // keep the shading across a model switch
    this.reset();
    this.resize();
  }

  // ---- exploded view: parts slide out from the model's centre ----

  prepareExplode() {
    const root = this.model;
    root.updateMatrixWorld(true);
    const center = new THREE.Box3().setFromObject(root).getCenter(new THREE.Vector3());
    const push = this.radius * 0.1; // so parts near the centre still separate
    let minY = 0;
    for (const m of this.meshes) {
      const box = new THREE.Box3().setFromObject(m);
      const c = box.getCenter(new THREE.Vector3());
      const dir = c.clone().sub(center).multiplyScalar(0.45);
      if (dir.lengthSq() > 1e-12) dir.addScaledVector(dir.clone().normalize(), push);
      minY = Math.min(minY, box.min.y + dir.y);
      // The offset is in world space; parts may sit under transformed parents.
      const p = m.parent;
      m.userData.home = m.position.clone();
      m.userData.dir = p.worldToLocal(c.clone().add(dir)).sub(p.worldToLocal(c.clone()));
    }
    this.lift = -minY; // keeps the lowest part on the floor
    this.rootHome = root.position.clone();
    this.explodeT = this.explodeTo = 0;
    this.camScale = 1;
  }

  get exploded() {
    return this.explodeTo === 1;
  }

  setExploded(on) {
    this.explodeTo = on ? 1 : 0;
    this.controls.autoRotate = false;
  }

  stepExplode() {
    if (this.explodeT === this.explodeTo) return;
    const d = this.explodeTo - this.explodeT;
    this.explodeT += Math.sign(d) * Math.min(Math.abs(d), 1 / 36); // ~0.6 s
    const e = this.explodeT * this.explodeT * (3 - 2 * this.explodeT); // smoothstep
    for (const m of this.meshes) m.position.copy(m.userData.home).addScaledVector(m.userData.dir, e);
    this.model.position.copy(this.rootHome).setY(this.rootHome.y + this.lift * e);
    // Pull the camera back and up with the parts, keeping the user's angle.
    const scale = 1 + 0.45 * e;
    const target = this.home.target.clone().setY(this.home.target.y + this.lift * e);
    const offset = this.camera.position.clone().sub(this.controls.target);
    offset.multiplyScalar(scale / this.camScale);
    this.camScale = scale;
    this.controls.target.copy(target);
    this.camera.position.copy(target).add(offset);
  }

  addShadow(size) {
    const c = document.createElement("canvas");
    c.width = c.height = 128;
    const g = c.getContext("2d");
    const grad = g.createRadialGradient(64, 64, 0, 64, 64, 64);
    grad.addColorStop(0, "rgba(0,0,0,0.55)");
    grad.addColorStop(1, "rgba(0,0,0,0)");
    g.fillStyle = grad;
    g.fillRect(0, 0, 128, 128);
    const s = Math.max(size.x, size.z) * 1.5;
    const plane = new THREE.Mesh(
      new THREE.PlaneGeometry(s, s),
      new THREE.MeshBasicMaterial({ map: new THREE.CanvasTexture(c), transparent: true, depthWrite: false })
    );
    plane.rotation.x = -Math.PI / 2;
    plane.position.y = 0.001 * this.radius;
    this.scene.add(plane);
    this.shadow = plane;
  }

  // clear removes the current model so another can be shown in the same
  // WebGL context (browsers cap how many a page may hold).
  clear() {
    for (const o of [this.model, this.shadow]) {
      if (!o) continue;
      this.scene.remove(o);
      o.traverse((n) => {
        n.geometry?.dispose();
        for (const m of [].concat(n.material || [], n.userData?.material || [])) {
          for (const v of Object.values(m)) if (v?.isTexture) v.dispose();
          m.dispose?.();
        }
      });
    }
    this.model = this.shadow = null;
    this.meshes = [];
  }

  reset() {
    const d = this.radius / Math.sin(THREE.MathUtils.degToRad(this.camera.fov / 2)) * 1.08 * (this.camScale || 1);
    const dir = new THREE.Vector3(0.62, 0.42, 1).normalize();
    const e = this.explodeT * this.explodeT * (3 - 2 * this.explodeT);
    this.controls.target.copy(this.home.target).setY(this.home.target.y + (this.lift || 0) * (e || 0));
    this.camera.position.copy(this.controls.target).add(dir.multiplyScalar(d));
    this.camera.near = d / 100;
    this.camera.far = d * 100;
    this.camera.updateProjectionMatrix();
    this.controls.autoRotate = true;
  }

  stats() {
    let tris = 0;
    let tex = 0;
    for (const m of this.meshes) {
      const g = m.geometry;
      tris += (g.index ? g.index.count : g.attributes.position.count) / 3;
      for (const mat of [].concat(m.userData.material)) {
        const img = mat?.map?.image;
        if (img) tex = Math.max(tex, img.width || 0, img.height || 0);
      }
    }
    return { tris: Math.round(tris), parts: this.meshes.length, tex };
  }

  parts() {
    const colors = PART_COLORS(this.meshes.length);
    return this.meshes.map((m, i) => ({ name: m.name || m.parent?.name || `part ${i + 1}`, color: `#${colors[i].getHexString()}`, mesh: m }));
  }

  setMode(mode) {
    this.mode = mode;
    const colors = PART_COLORS(this.meshes.length);
    this.meshes.forEach((m, i) => {
      if (mode === "textured") m.material = m.userData.material;
      else
        m.material = new THREE.MeshStandardMaterial({
          color: mode === "parts" ? colors[i] : 0xc9c4ba,
          roughness: 0.62,
          metalness: 0,
          flatShading: false,
        });
    });
    this.applyWire();
  }

  setWire(on) {
    this.wire = on;
    this.applyWire();
  }

  applyWire() {
    for (const m of this.meshes) {
      if (!m.userData.wire) {
        // Dark lines read on white clay, light and mid textures alike.
        m.userData.wire = new THREE.LineSegments(
          new THREE.WireframeGeometry(m.geometry),
          new THREE.LineBasicMaterial({ color: 0x101014, transparent: true, opacity: 0.5 })
        );
        m.add(m.userData.wire);
      }
      m.userData.wire.visible = this.wire;
      // Push the faces back a little so the lines win the depth test.
      for (const mat of [].concat(m.material)) {
        mat.polygonOffset = this.wire;
        mat.polygonOffsetFactor = mat.polygonOffsetUnits = this.wire ? 1 : 0;
        mat.needsUpdate = true;
      }
    }
  }

  togglePart(mesh) {
    mesh.visible = !mesh.visible;
    return mesh.visible;
  }
}
