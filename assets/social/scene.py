import math
from pathlib import Path
import sys
import xml.etree.ElementTree as ET

import bpy
from mathutils import Vector

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import layout

PX = 0.01
HEIGHT = 8 * PX
BEVEL = 8 * PX
ELEVATION = math.radians(14)
PAPER = (0.0124, 0.0292, 0.0533)


def slab(name, verts, material):
    mesh = bpy.data.meshes.new(name)
    mesh.from_pydata(verts, [], [list(range(len(verts)))])
    obj = bpy.data.objects.new(name, mesh)
    bpy.context.collection.objects.link(obj)
    solid = obj.modifiers.new("solid", "SOLIDIFY")
    solid.thickness = HEIGHT
    solid.offset = 1.0 if mesh.polygons[0].normal.z > 0 else -1.0
    bevel = obj.modifiers.new("bevel", "BEVEL")
    bevel.width = BEVEL
    bevel.segments = 10
    bevel.limit_method = "ANGLE"
    bevel.angle_limit = math.radians(40)
    obj.data.materials.append(material)


def material(name, color, *, lit=False, spec=0.0, rough=0.9):
    mat = bpy.data.materials.new(name)
    nodes = mat.node_tree.nodes
    bsdf = nodes["Principled BSDF"]
    bsdf.inputs["Base Color"].default_value = (*color, 1)
    bsdf.inputs["Roughness"].default_value = rough
    bsdf.inputs["Specular IOR Level"].default_value = spec
    if lit:
        bsdf.inputs["Emission Color"].default_value = (*color, 1)
        # Lower the emission on camera rays so the dot lights the groove without looking blown out.
        light_path = nodes.new("ShaderNodeLightPath")
        strength = nodes.new("ShaderNodeMapRange")
        strength.inputs["To Min"].default_value = 1.5
        strength.inputs["To Max"].default_value = 0.25
        mat.node_tree.links.new(light_path.outputs["Is Camera Ray"], strength.inputs["Value"])
        mat.node_tree.links.new(strength.outputs["Result"], bsdf.inputs["Emission Strength"])
    return mat


def add_relief(scene):
    logo = HERE.parent / "logo-dark.svg"
    svg = ET.parse(logo).getroot()
    svg_width, svg_height = (float(svg.attrib[key]) for key in ("width", "height"))

    bpy.ops.preferences.addon_enable(module="io_curve_svg")
    bpy.ops.import_curve.svg(filepath=str(logo))
    # The mark contains the cell and then the dot. The importer resolves their fill colours.
    cell, dot = scene.objects
    accent = tuple(dot.data.materials[0].diffuse_color[:3])

    # The importer maps SVG pixels at 90 dpi from the lower left of the viewport.
    svg_pixel = 0.0254 / 90
    scale = layout.RELIEF_VIEWPORT_WIDTH_PX * PX / (svg_width * svg_pixel)
    left = layout.RELIEF_VIEWPORT_LEFT_PX * PX
    bottom = -layout.RELIEF_VIEWPORT_TOP_PX * PX - svg_height * svg_pixel * scale

    relief = material("relief", tuple(v * 1.4 for v in PAPER), spec=1.0, rough=0.35)
    lamp = material("dot", accent, lit=True, spec=1.0, rough=0.35)
    for curve, name, mat in ((cell, "cell", relief), (dot, "dot", lamp)):
        # A 3D curve with no extrusion or bevel evaluates to the outline alone.
        curve.data.dimensions = "3D"
        curve.data.resolution_u = 90
        outline = curve.to_mesh()
        verts = [(left + v.co.x * scale, bottom + v.co.y * scale, 0.0) for v in outline.vertices]
        curve.to_mesh_clear()
        bpy.data.objects.remove(curve, do_unlink=True)
        slab(name, verts, mat)


def render(height, output):
    bpy.ops.wm.read_factory_settings(use_empty=True)
    scene = bpy.context.scene
    paper = material("paper", PAPER)
    add_relief(scene)

    bpy.ops.mesh.primitive_plane_add(size=60, location=(6.4, -3.2, 0))
    bpy.context.object.data.materials.append(paper)

    sun = bpy.data.lights.new("sun", "SUN")
    sun.energy = 3.0
    sun.angle = math.radians(4)
    sun.color = (0.80, 0.88, 1.0)
    light = bpy.data.objects.new("sun", sun)
    bpy.context.collection.objects.link(light)
    diagonal = math.cos(ELEVATION) / math.sqrt(2)
    travel = Vector((diagonal, -diagonal, -math.sin(ELEVATION)))
    light.rotation_euler = travel.to_track_quat("-Z", "Y").to_euler()

    world = bpy.data.worlds.new("world")
    background = world.node_tree.nodes["Background"]
    background.inputs["Color"].default_value = (0.55, 0.7, 1.0, 1)
    background.inputs["Strength"].default_value = 0.10
    scene.world = world

    camera = bpy.data.cameras.new("camera")
    camera.type = "ORTHO"
    camera.ortho_scale = layout.CANVAS_WIDTH_PX * PX
    camera_obj = bpy.data.objects.new("camera", camera)
    camera_obj.location = (layout.CANVAS_WIDTH_PX * PX / 2, -height * PX / 2, 10)
    bpy.context.collection.objects.link(camera_obj)
    scene.camera = camera_obj

    scene.render.engine = "CYCLES"
    scene.cycles.device = "CPU"
    scene.cycles.samples = 1024
    scene.cycles.use_denoising = False
    scene.cycles.max_bounces = 4
    scene.render.resolution_x = layout.CANVAS_WIDTH_PX
    scene.render.resolution_y = height
    scene.render.resolution_percentage = 100
    scene.view_settings.view_transform = "Standard"
    scene.view_settings.look = "None"
    scene.render.image_settings.file_format = "PNG"
    scene.render.image_settings.color_depth = "16"
    scene.render.image_settings.color_mode = "RGB"
    scene.render.filepath = str(Path(output).resolve())
    bpy.ops.render.render(write_still=True)


if __name__ == "__main__":
    height, output = sys.argv[sys.argv.index("--") + 1:]
    render(int(height), output)
