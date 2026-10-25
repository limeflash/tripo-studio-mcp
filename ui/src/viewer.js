// 3D preview for the card: loads a GLB (or FBX) from the server's local
// preview URL, frames it, and offers textured / clay / parts views. No
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
    this.stage = stage;
    this.meshes = [];
    this.mode = "textured";
    this.wire = false;

    const renderer = (this.renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true }));
    renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
    renderer.outputColorSpace = THREE.SRGBColorSpace;
    renderer.toneMapping = THREE.ACESFilmicToneMapping;
    renderer.toneMappingExposure = 1.05;
    stage.prepend(renderer.domElement);

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

    new ResizeObserver(() => this.resize()).observe(stage);
    this.visible = true;
    new IntersectionObserver(([e]) => (this.visible = e.isIntersecting)).observe(stage);
    const tick = () => {
      requestAnimationFrame(tick);
      if (!this.visible || !this.model) return;
      controls.update();
      renderer.render(scene, this.camera);
    };
    tick();
  }

  resize() {
    const { clientWidth: w, clientHeight: h } = this.stage;
    if (!w || !h) return;
    this.renderer.setSize(w, h, false);
    this.camera.aspect = w / h;
    this.camera.updateProjectionMatrix();
  }

  async load(url) {
    const res = await fetch(url);
    if (!res.ok) throw new Error((await res.text()) || `HTTP ${res.status}`);
    const buf = await res.arrayBuffer();
    const magic = new TextDecoder().decode(new Uint8Array(buf, 0, 4));
    let root;
    if (magic === "glTF") {
      root = (await new GLTFLoader().parseAsync(buf, "")).scene;
    } else if (res.headers.get("X-Model-Format") === "fbx") {
      root = new FBXLoader().parse(buf, "");
    } else {
      throw new Error("not a GLB or FBX model");
    }
    this.setModel(root);
  }

  setModel(root) {
    root.traverse((o) => {
      if (!o.isMesh) return;
      o.userData.material = o.material;
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
    this.reset();
    this.resize();
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
  }

  reset() {
    const d = this.radius / Math.sin(THREE.MathUtils.degToRad(this.camera.fov / 2)) * 1.08;
    const dir = new THREE.Vector3(0.62, 0.42, 1).normalize();
    this.controls.target.copy(this.home.target);
    this.camera.position.copy(this.home.target).add(dir.multiplyScalar(d));
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
        m.userData.wire = new THREE.LineSegments(
          new THREE.WireframeGeometry(m.geometry),
          new THREE.LineBasicMaterial({ color: 0xffffff, transparent: true, opacity: 0.16, depthTest: true })
        );
        m.add(m.userData.wire);
      }
      m.userData.wire.visible = this.wire;
    }
  }

  togglePart(mesh) {
    mesh.visible = !mesh.visible;
    return mesh.visible;
  }
}
